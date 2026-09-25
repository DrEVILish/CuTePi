package routes

import (
	"math/rand"
	"strconv"
	"sync"
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// Awards Mode (§12.13): a group playback toggle for ceremonies. The operator
// parks the selection on the group and each GO alternates play → fade-stop
// on a single member, without the selection ever leaving the header.
//
// State machine per group (in-memory only — a show reload starts fresh):
//
//	idle → GO → playing(member) → GO → stopping → (fade ends) → idle,
//	                                     ↘ (operator played something else
//	                                        mid-fade) → dropped, no advance.
//
// The order list is the member snapshot in sheet order, or a shuffled
// no-repeat bag of it; loop wraps (reshuffling), otherwise the stop of the
// last member steps the selection out of the group so the next GO continues
// the show normally.

type awardsPhase int

const (
	awardsIdle awardsPhase = iota
	awardsPlaying
	awardsStopping
)

type awardsSession struct {
	groupID int
	members []int // snapshot in sheet order (subtree, like playFirstGroupMember)
	order   []int // play order: members or a shuffled bag of positions
	pos     int   // index of the next member to play
	phase   awardsPhase
	member  int // cuePos currently playing (playing/stopping only)
	shuffle bool
	loop    bool // order/loop semantics the session was built under
}

var (
	awardsMu  sync.Mutex
	awardsSt  awardsSession
	awardsRnd = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// awardsOrder returns the play order for a member snapshot: sheet order, or
// a shuffled no-repeat bag when the group's shuffle is on. Pure (testable);
// the caller owns locking.
func awardsOrder(members []int, shuffle bool, r *rand.Rand) []int {
	order := append([]int(nil), members...)
	if shuffle {
		r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	}
	return order
}

// awardsMembers snapshots the group's playable members in sheet order
// (subtree scope, same as playFirstGroupMember).
func awardsMembers(groupID int) []int {
	sheet, err := ctp.GetCuesheet()
	if err != nil {
		return nil
	}
	inScope := groupScope(groupID, &sheet)
	var out []int
	for _, cue := range sheet.Cues {
		if inScope[cue.Parent] {
			out = append(out, cue.CuePos)
		}
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// awardsGO runs one GO press on an awards-mode group. It always owns the
// selection (no advance, no prewarm) — the caller skips those.
func awardsGO(g ctp.Group) error {
	// Fresh settings every press: the inspector may have changed shuffle /
	// loop / fade between GOs.
	if fresh, err := ctp.GetGroup(g.GroupID); err == nil {
		g = fresh
	}
	members := awardsMembers(g.GroupID)
	awardsMu.Lock()
	// New session when the group changed, the membership changed, or the
	// mode settings changed (order/loop semantics come from the flags).
	if awardsSt.groupID != g.GroupID || !sameInts(awardsSt.members, members) ||
		awardsSt.shuffle != g.Shuffle || awardsSt.loop != g.Loop {
		awardsSt = awardsSession{groupID: g.GroupID, members: members, shuffle: g.Shuffle, loop: g.Loop}
	}
	if len(members) == 0 {
		awardsMu.Unlock()
		return nil
	}
	if len(awardsSt.order) == 0 {
		awardsSt.order = awardsOrder(members, g.Shuffle, awardsRnd)
		awardsSt.pos = 0
	}
	switch awardsSt.phase {
	case awardsStopping:
		// A GO arriving mid-fade is ignored: the stop already owns the
		// next state.
		awardsMu.Unlock()
		return nil
	case awardsPlaying:
		// The engine moved on without us (direct fire, stop, panic,
		// auto-continue elsewhere, or the member ended on its own): the
		// session restarts from the cursor instead of stopping thin air.
		if gsp.CurrentCuePos() != awardsSt.member {
			awardsSt.phase = awardsIdle
			break
		}
		member, fade := awardsSt.member, g.FadeMS
		awardsSt.phase = awardsStopping
		awardsMu.Unlock()
		goSafe(func() {
			gsp.FadeAndStop(fade)
			awardsAfterStop(g.GroupID, member)
		})
		return nil
	}
	// Idle: play the cursor member. The cursor only advances on a
	// successful fire, so a failed load retries the same member. A cursor
	// past the end here (the stop path owns end-of-list; this is only
	// reachable via a raced reset) restarts the order rather than panic.
	if awardsSt.pos >= len(awardsSt.order) {
		awardsSt.order = awardsOrder(members, g.Shuffle, awardsRnd)
		awardsSt.pos = 0
	}
	idx := awardsSt.pos
	pos := awardsSt.order[idx]
	awardsMu.Unlock()
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		return err
	}
	if err := gsp.LoadWithOpts(cue.Filename, cueOpts(cue, false)); err != nil {
		ctp.SetCueResult(cue.CuePos, ctp.CueResultError)
		return err
	}
	ctp.SetCueResult(cue.CuePos, ctp.CueResultOK)
	gsp.SetCuePos(cue.CuePos)
	gsp.Play()
	logs.Emit(logs.AuditEvent{Event: "cue_start", Pos: cue.CuePos, Title: cue.Title})
	awardsMu.Lock()
	// The session may have been reset while we loaded (selection moved),
	// or a concurrent GO may have fired first: only claim the playing
	// state if the cursor is still where we left it.
	if awardsSt.groupID == g.GroupID && awardsSt.phase == awardsIdle && awardsSt.pos == idx {
		awardsSt.phase = awardsPlaying
		awardsSt.member = cue.CuePos
		awardsSt.pos++
	}
	awardsMu.Unlock()
	return nil
}

// awardsAfterStop runs when the stop-fade for member finishes. It advances
// the cursor; at the end of the list it wraps (loop, reshuffling a shuffled
// bag) or steps the selection out of the group so the next GO continues the
// show normally. A non-empty engine (the operator started something during
// the fade) means the session is dropped, not advanced.
func awardsAfterStop(groupID, member int) {
	awardsMu.Lock()
	if awardsSt.groupID != groupID || awardsSt.phase != awardsStopping || awardsSt.member != member {
		awardsMu.Unlock()
		return
	}
	if gsp.CurrentCuePos() != 0 {
		// Operator-owned now: reset the session, leave selection alone.
		awardsSt = awardsSession{}
		awardsMu.Unlock()
		return
	}
	g, err := ctp.GetGroup(groupID)
	if err != nil {
		awardsSt = awardsSession{}
		awardsMu.Unlock()
		return
	}
	if awardsSt.pos < len(awardsSt.order) {
		awardsSt.phase = awardsIdle
		awardsMu.Unlock()
		return
	}
	// End of list.
	if g.Loop {
		awardsSt.order = awardsOrder(awardsSt.members, g.Shuffle, awardsRnd)
		awardsSt.pos = 0
		awardsSt.phase = awardsIdle
		awardsMu.Unlock()
		return
	}
	members := append([]int(nil), awardsSt.members...)
	awardsSt = awardsSession{}
	awardsMu.Unlock()
	// Step out of the group block, then fire the next unit like a normal
	// GO (this move IS the advance, so no further goAdvance step follows).
	// A following group header parks the selection (no auto-fire into it).
	if pos := awardsStepOut(groupID, members); pos != 0 {
		if cue, cerr := ctp.GetCue(strconv.Itoa(pos)); cerr == nil {
			if lerr := loadAndPlayCue(cue); lerr != nil {
				logs.Printf(logs.RTECuePlay, "awards continue-out failed pos=%d error=%v", pos, lerr)
			} else if next, nerr := ctp.NextCuePos(pos); nerr == nil && next != 0 {
				// Deck-style preload for whatever follows, like a normal GO.
				armNextCue(gsp.Generation(), next)
			}
		}
	}
}

// awardsStepOut moves the selection past the group block to the next unit
// outside the member snapshot: SelectStep(1) alone would land on the first
// member of an expanded group. Returns the exit cue's pos, or 0 when the
// selection parked on another header or nowhere (end of sheet — then the
// selection rests back on the group header).
func awardsStepOut(groupID int, members []int) int {
	inGroup := make(map[int]bool, len(members))
	for _, m := range members {
		inGroup[m] = true
	}
	// Break on no-progress: SelectStep clamps (nil, no move) at the sheet
	// end, and without the guard this spins 64 wasted steps.
	lastGid, lastPos := -1, -1
	for i := 0; i < 64; i++ {
		if err := ctp.SelectStep(1); err != nil {
			break
		}
		gid, _ := ctp.SelectedGroupPos()
		pos, _ := ctp.SelectedCuePos()
		if gid == lastGid && pos == lastPos {
			break
		}
		lastGid, lastPos = gid, pos
		if gid != 0 {
			return 0
		}
		if pos == 0 {
			break
		}
		if !inGroup[pos] {
			return pos
		}
	}
	_ = ctp.SetSelectedGroup(groupID)
	return 0
}

// awardsSuppressAutoContinue reports whether the ending cue is an awards
// session member: the cue-end auto-continue chain must never fire for one
// (awards cues always wait for the operator, even with AutoContinue set).
func awardsSuppressAutoContinue(cuePos int) bool {
	awardsMu.Lock()
	defer awardsMu.Unlock()
	return awardsSt.groupID != 0 && awardsSt.phase == awardsPlaying && awardsSt.member == cuePos
}

// awardsSelectionLeft resets the session when the selection moves to another
// unit. Playback is untouched (leaving never stops audio); the next GO on
// the group starts fresh.
func awardsSelectionLeft(selectedGroupID int) {
	awardsMu.Lock()
	defer awardsMu.Unlock()
	if awardsSt.groupID != 0 && awardsSt.groupID != selectedGroupID {
		awardsSt = awardsSession{}
	}
}

// awardsSelectionSync resets the session unless the selection is still on
// the session's group. Call after any selection mutation.
func awardsSelectionSync() {
	gid, _ := ctp.SelectedGroupPos()
	awardsSelectionLeft(gid)
}
