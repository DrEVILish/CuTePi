package ctp

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/jmoiron/sqlx"
	"math"
	"strconv"
	"strings"
)

// ErrDuplicateCueNum marks a cue number that another cue or group header
// already uses. It is an operator mistake, not a fault: callers reject the
// edit and leave the old number in place without logging an error.
var ErrDuplicateCueNum = errors.New("cue number already in use")

type duplicateCueNumError struct{ num string }

func (e *duplicateCueNumError) Error() string {
	return fmt.Sprintf("Cue number %s is already used", e.num)
}
func (e *duplicateCueNumError) Is(target error) bool { return target == ErrDuplicateCueNum }

// sameCueNum treats "12" and "12.0" as the same number; text numbers compare
// case-insensitively.
func sameCueNum(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if strings.EqualFold(a, b) {
		return true
	}
	fa, aok := parseCueNumFloat(a)
	fb, bok := parseCueNumFloat(b)
	return aok && bok && fa == fb
}

// otherCueNums lists every cue and group-header number except the cue at
// exceptCuePos and the group exceptGroupID (0 = none).
func otherCueNums(q sqlx.Queryer, exceptCuePos, exceptGroupID int) ([]string, error) {
	var nums []string
	err := sqlx.Select(q, &nums, `
		SELECT COALESCE(cueNum, '') FROM cuesheet WHERE cuePos != ?
		UNION ALL
		SELECT COALESCE(cue_num, '') FROM cue_group WHERE group_id != ?`, exceptCuePos, exceptGroupID)
	return nums, err
}

// checkCueNumFree returns a duplicateCueNumError when num is already used.
// Blank numbers never collide.
func checkCueNumFree(q sqlx.Queryer, num string, exceptCuePos, exceptGroupID int) error {
	num = strings.TrimSpace(num)
	if num == "" {
		return nil
	}
	nums, err := otherCueNums(q, exceptCuePos, exceptGroupID)
	if err != nil {
		return err
	}
	for _, n := range nums {
		if sameCueNum(n, num) {
			return &duplicateCueNumError{num}
		}
	}
	return nil
}

// maxCueNum is the highest numeric cue or group-header number (0 if none).
func maxCueNum(q sqlx.Queryer) (float64, error) {
	nums, err := otherCueNums(q, 0, 0)
	if err != nil {
		return 0, err
	}
	max := 0.0
	for _, n := range nums {
		if f, ok := parseCueNumFloat(n); ok && f > max {
			max = f
		}
	}
	return max, nil
}

// freeCueNum keeps num when it is unused, otherwise hands out the next whole
// number above every existing one (imports appended onto a sheet).
func freeCueNum(q sqlx.Queryer, num string) (string, error) {
	err := checkCueNumFree(q, num, 0, 0)
	if !errors.Is(err, ErrDuplicateCueNum) {
		return num, err
	}
	max, err := maxCueNum(q)
	if err != nil {
		return "", err
	}
	return formatCueNum(math.Floor(max) + 1), nil
}

// currentCueNum reads the stored number of one cue.
func currentCueNum(cuePos int) (string, error) {
	var cur string
	err := db.Get(&cur, `SELECT COALESCE(cueNum, '') FROM cuesheet WHERE cuePos = ?`, cuePos)
	return cur, err
}

// parseCueNumFloat parses a cueNum that is a plain decimal number ("12",
// "12.5"); ok=false for hand-set text numbers, which auto-numbering never
// touches.
func parseCueNumFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// formatCueNum renders a cue number without a trailing ".0" (12 → "12",
// 12.5 → "12.5").
func formatCueNum(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}

// nextCueNum computes the cue number for a new cue landing at visual
// position sheetIndex. With auto-numbering on: an append gets the next whole
// number; an insert between two numeric neighbours (in sheet order, the
// order the operator sees) gets their midpoint ("12.5" between 12 and 13).
// Off (or non-numeric neighbours) falls back to MAX+1.
func nextCueNum(tx *sqlx.Tx, sheetIndex float64, auto bool, step float64) (string, error) {
	maxNum, err := maxCueNum(tx)
	if err != nil {
		return "", err
	}
	// The next multiple of step above the highest number (step 1: MAX+1).
	next := (math.Floor(maxNum/step) + 1) * step
	if !auto {
		return formatCueNum(next), nil
	}
	{
		var prevNum, nextNum sql.NullString
		_ = tx.Get(&prevNum, `SELECT cueNum FROM cuesheet WHERE sheet_index < ? ORDER BY sheet_index DESC, cuePos DESC LIMIT 1`, sheetIndex)
		_ = tx.Get(&nextNum, `SELECT cueNum FROM cuesheet WHERE sheet_index > ? ORDER BY sheet_index, cuePos LIMIT 1`, sheetIndex)
		p, pok := parseCueNumFloat(prevNum.String)
		n, nok := parseCueNumFloat(nextNum.String)
		if pok && nok && n > p {
			// Walk up from the midpoint in 0.5 steps until the number is
			// free (the UNIQUE constraint makes collision a hard error).
			mid := (p + n) / 2
			for step := 0.0; step < 50; step += 0.5 {
				cand := formatCueNum(mid + step)
				err := checkCueNumFree(tx, cand, 0, 0)
				if err == nil {
					return cand, nil
				}
				if !errors.Is(err, ErrDuplicateCueNum) {
					return "", err
				}
			}
		}
	}
	// Append (or unparseable neighbours): the next whole number above the
	// max (12.5 → 13), so appends keep a clean integer sequence.
	return formatCueNum(next), nil
}
