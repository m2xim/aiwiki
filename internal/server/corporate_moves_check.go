package server

import "log"

// warnMovedArticleMismatch logs (never fails startup) the number of articles whose
// space_id differs from their project's space. Such rows are invisible to every
// user; they can only appear if the move guards were bypassed by hand.
func (d *database) warnMovedArticleMismatch() {
	var n int
	if err := d.QueryRow(`SELECT corporate_move_mismatch_count()`).Scan(&n); err != nil {
		log.Printf("warning: could not check article/project space consistency: %v", err)
		return
	}
	if n > 0 {
		log.Printf("warning: %d article(s) have a space_id that differs from their project's space and are invisible; repair them as the database owner", n)
	}
}
