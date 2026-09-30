package ctp

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// stateKeyCueNumStep is the auto-numbering increment (§12.5, default 1):
// appends take the next multiple above the highest number, and Renumber
// writes step, 2×step, 3×step… in sheet order.
const stateKeyCueNumStep = "cueNumStep"

const DefaultCueNumStep = 1.0

// GetCueNumStep returns the cue-number increment (default 1).
func GetCueNumStep() float64 {
	var val string
	if err := db.Get(&val, `SELECT value FROM state WHERE key = ?`, stateKeyCueNumStep); err != nil {
		return DefaultCueNumStep
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return DefaultCueNumStep
	}
	return f
}

// ValidateCueNumStep accepts any positive number.
func ValidateCueNumStep(step float64) error {
	if step <= 0 || math.IsInf(step, 0) || math.IsNaN(step) {
		return errors.New("cue number step must be a positive number")
	}
	return nil
}

// SetCueNumStep persists the cue-number increment.
func SetCueNumStep(step float64) error {
	if err := ValidateCueNumStep(step); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value;`, stateKeyCueNumStep, formatCueNum(step))
	return err
}

// RenumberSheet rewrites every cue number in visual sheet order as step,
// 2×step, 3×step… (1, 2, 3… at the default step). A group header that has a
// number takes the next one in the same sequence, so a header can never
// collide with a cue; blank headers stay blank. Explicit operator action:
// hand-set numbers are replaced.
func RenumberSheet() error {
	step := GetCueNumStep()
	seq, err := loadSheetSequence()
	if err != nil {
		return err
	}
	byID, _, err := groupNesting()
	if err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// cuesheet.cueNum is UNIQUE: park every number on a free value first.
	if _, err := tx.Exec(`UPDATE cuesheet SET cueNum = '~renumber-' || cuePos`); err != nil {
		return err
	}
	n := 0
	for _, it := range seq {
		if it.Kind == "group" {
			if strings.TrimSpace(byID[it.GroupID].CueNum) == "" {
				continue
			}
			n++
			if _, err := tx.Exec(`UPDATE cue_group SET cue_num = ? WHERE group_id = ?`, formatCueNum(float64(n)*step), it.GroupID); err != nil {
				return err
			}
			continue
		}
		n++
		if _, err := tx.Exec(`UPDATE cuesheet SET cueNum = ? WHERE cuePos = ?`, formatCueNum(float64(n)*step), it.CuePos); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	bumpCuesheetVersion()
	return nil
}

// cueNumSortKey orders numbers naturally: numeric first by value, then text
// numbers alphabetically, blanks last.
type cueNumSortKey struct {
	class int // 0 numeric, 1 text, 2 blank
	num   float64
	text  string
}

func sortKeyOf(num string) cueNumSortKey {
	num = strings.TrimSpace(num)
	if num == "" {
		return cueNumSortKey{class: 2}
	}
	if f, ok := parseCueNumFloat(num); ok {
		return cueNumSortKey{class: 0, num: f}
	}
	return cueNumSortKey{class: 1, text: strings.ToLower(num)}
}

func (a cueNumSortKey) less(b cueNumSortKey) bool {
	if a.class != b.class {
		return a.class < b.class
	}
	if a.class == 0 {
		return a.num < b.num
	}
	return a.text < b.text
}

// SortSheetByCueNumber reorders the sheet by cue number. Sorting happens
// within each group (and at top level): members stay in their group, a
// group's block moves as one, and a header without a number sorts by the
// lowest number inside it. Ties keep their current order.
func SortSheetByCueNumber() error {
	seq, err := loadSheetSequence()
	if err != nil {
		return err
	}
	parents, err := storedParents()
	if err != nil {
		return err
	}
	byID, _, err := groupNesting()
	if err != nil {
		return err
	}
	var cueNums []struct {
		CuePos int    `db:"cuePos"`
		CueNum string `db:"cueNum"`
	}
	if err := db.Select(&cueNums, `SELECT cuePos, COALESCE(cueNum, '') AS cueNum FROM cuesheet`); err != nil {
		return err
	}
	numOf := make(map[int]string, len(cueNums))
	for _, r := range cueNums {
		numOf[r.CuePos] = r.CueNum
	}
	scopeOf := func(it SheetItem) int {
		if it.Kind == "group" {
			return byID[it.GroupID].ParentGroupID
		}
		return parents[it.CuePos]
	}
	kids := map[int][]SheetItem{}
	for _, it := range seq {
		kids[scopeOf(it)] = append(kids[scopeOf(it)], it)
	}
	keys := map[SheetItem]cueNumSortKey{}
	var keyOf func(it SheetItem, depth int) cueNumSortKey
	keyOf = func(it SheetItem, depth int) cueNumSortKey {
		if k, ok := keys[it]; ok {
			return k
		}
		var k cueNumSortKey
		if it.Kind == "cue" {
			k = sortKeyOf(numOf[it.CuePos])
		} else if k = sortKeyOf(byID[it.GroupID].CueNum); k.class == 2 && depth <= len(byID) {
			for _, child := range kids[it.GroupID] {
				if ck := keyOf(child, depth+1); ck.less(k) {
					k = ck
				}
			}
		}
		keys[it] = k
		return k
	}
	var out []SheetItem
	seen := map[SheetItem]bool{}
	var walk func(scope, depth int)
	walk = func(scope, depth int) {
		list := append([]SheetItem(nil), kids[scope]...)
		sort.SliceStable(list, func(i, j int) bool { return keyOf(list[i], 0).less(keyOf(list[j], 0)) })
		for _, it := range list {
			if seen[it] {
				continue
			}
			seen[it] = true
			out = append(out, it)
			if it.Kind == "group" && depth <= len(byID) {
				walk(it.GroupID, depth+1)
			}
		}
	}
	walk(0, 0)
	// Rows whose parent chain is broken (not reachable from the top) keep
	// their relative order at the end rather than vanishing.
	for _, it := range seq {
		if !seen[it] {
			out = append(out, it)
		}
	}
	if len(out) != len(seq) {
		return fmt.Errorf("sort by cue number: %d rows became %d", len(seq), len(out))
	}
	return applyOrder(out, parents)
}
