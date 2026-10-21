package server

import "database/sql"

// moveDescendants moves the current subtree, including deleted articles, in the
// root's transaction. UNION visits each ID once even if stored data has a cycle.
func moveDescendants(tx *sql.Tx, root Article) error {
	rows, err := tx.Query(`WITH RECURSIVE subtree(id) AS (
		SELECT id FROM articles WHERE id=?
		UNION
		SELECT a.id FROM articles a JOIN subtree s ON a.parent_id=s.id
	) SELECT `+articleColumns+` FROM articles WHERE id IN (SELECT id FROM subtree) AND id<>? AND space_id<>?`, root.ID, root.ID, root.SpaceID)
	if err != nil {
		return err
	}
	var descendants []Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			rows.Close()
			return err
		}
		descendants = append(descendants, a)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, a := range descendants {
		a.SpaceID = root.SpaceID
		a.Revision++
		a.UpdatedAt = root.UpdatedAt
		a.Author = root.Author
		if _, err = tx.Exec(`UPDATE articles SET space_id=?,revision=?,updated_at=?,author=? WHERE id=?`, a.SpaceID, a.Revision, a.UpdatedAt, a.Author, a.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)`, a.ID, a.Revision, a.Author, "Перенос вместе с родительской статьёй", a.UpdatedAt, encode(a)); err != nil {
			return err
		}
		if a.DeletedAt != "" {
			_, err = tx.Exec(`DELETE FROM article_fts WHERE article_id=?`, a.ID)
		} else {
			err = indexArticle(tx, a)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
