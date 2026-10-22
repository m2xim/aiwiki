package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

type Space struct {
	Limited     bool   `json:"limited,omitempty"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ArchivedAt  string `json:"archived_at,omitempty"`
}
type Article struct {
	ID         string            `json:"id"`
	SpaceID    string            `json:"space_id"`
	ProjectID  string            `json:"project_id,omitempty"`
	ParentID   string            `json:"parent_id"`
	Title      string            `json:"title"`
	Summary    string            `json:"summary"`
	Body       string            `json:"body"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status"`
	Pinned     bool              `json:"pinned"`
	Tags       []string          `json:"tags"`
	Properties map[string]string `json:"properties"`
	Revision   int               `json:"revision"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
	Author     string            `json:"author"`
	DeletedAt  string            `json:"deleted_at,omitempty"`
}
type Revision struct {
	ID        int64   `json:"id"`
	ArticleID string  `json:"article_id"`
	Revision  int     `json:"revision"`
	Author    string  `json:"author"`
	Message   string  `json:"message"`
	CreatedAt string  `json:"created_at"`
	Snapshot  Article `json:"snapshot"`
}
type Comment struct {
	ID        string `json:"id"`
	ArticleID string `json:"article_id"`
	Anchor    string `json:"anchor"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Resolved  bool   `json:"resolved"`
}
type Attachment struct {
	ID        string `json:"id"`
	ArticleID string `json:"article_id"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
	DeletedAt string `json:"deleted_at,omitempty"`
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339) }
func randomSuffix() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
func encode(v any) string { b, _ := json.Marshal(v); return string(b) }

const articleColumns = `id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at`

func scanArticle(row interface{ Scan(...any) error }) (Article, error) {
	var a Article
	var pinned int
	var tags, props string
	var deletedAt sql.NullString
	err := row.Scan(&a.ID, &a.SpaceID, &a.ParentID, &a.ProjectID, &a.Title, &a.Summary, &a.Body, &a.Kind, &a.Status, &pinned, &tags, &props, &a.Revision, &a.CreatedAt, &a.UpdatedAt, &a.Author, &deletedAt)
	if err != nil {
		return a, err
	}
	a.Pinned = pinned != 0
	if deletedAt.Valid {
		a.DeletedAt = deletedAt.String
	}
	a.Tags = []string{}
	a.Properties = map[string]string{}
	_ = json.Unmarshal([]byte(tags), &a.Tags)
	_ = json.Unmarshal([]byte(props), &a.Properties)
	if a.Tags == nil {
		a.Tags = []string{}
	}
	if a.Properties == nil {
		a.Properties = map[string]string{}
	}
	return a, nil
}
func (s *Server) articleByID(q interface{ QueryRow(string, ...any) *sql.Row }, id string) (Article, error) {
	query := "SELECT " + articleColumns + " FROM articles WHERE id=?"
	if s.DB.pg {
		if _, ok := q.(*transaction); ok {
			query += " FOR UPDATE"
		}
	}
	return scanArticle(q.QueryRow(query, id))
}
func requiredArticle(a Article) error {
	if a.SpaceID == "" || a.Title == "" {
		return fmt.Errorf("space_id and title are required")
	}
	if utf8.RuneCountInString(a.Title) > 300 {
		return fmt.Errorf("title exceeds 300 characters")
	}
	if len(a.Body) > 2*1024*1024 {
		return fmt.Errorf("body exceeds 2 MB")
	}
	switch a.Kind {
	case "article", "instruction", "research", "plan", "source":
	default:
		return fmt.Errorf("invalid kind")
	}
	switch a.Status {
	case "draft", "active", "verified", "archived":
	default:
		return fmt.Errorf("invalid status")
	}
	return nil
}

func (s *Server) migrate() error {
	if s.DB.pg {
		return s.migratePostgres()
	}
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS spaces(id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',archived_at TEXT);
CREATE TABLE IF NOT EXISTS articles(id TEXT PRIMARY KEY,space_id TEXT NOT NULL REFERENCES spaces(id),parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL,summary TEXT NOT NULL DEFAULT '',body TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,status TEXT NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,tags TEXT NOT NULL DEFAULT '[]',properties TEXT NOT NULL DEFAULT '{}',revision INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,author TEXT NOT NULL,deleted_at TEXT);
CREATE INDEX IF NOT EXISTS articles_space_updated ON articles(space_id,updated_at DESC); CREATE INDEX IF NOT EXISTS articles_parent ON articles(parent_id);
CREATE TABLE IF NOT EXISTS revisions(id INTEGER PRIMARY KEY AUTOINCREMENT,article_id TEXT NOT NULL REFERENCES articles(id),revision INTEGER NOT NULL,author TEXT NOT NULL,message TEXT NOT NULL,created_at TEXT NOT NULL,snapshot TEXT NOT NULL,UNIQUE(article_id,revision));
CREATE TABLE IF NOT EXISTS comments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),anchor TEXT NOT NULL,body TEXT NOT NULL,author TEXT NOT NULL,created_at TEXT NOT NULL,resolved INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS attachments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),name TEXT NOT NULL,mime TEXT NOT NULL,size INTEGER NOT NULL,stored_name TEXT NOT NULL,created_at TEXT NOT NULL,deleted_at TEXT);
CREATE TABLE IF NOT EXISTS favorites(article_id TEXT PRIMARY KEY REFERENCES articles(id),created_at TEXT NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS article_fts USING fts5(article_id UNINDEXED,title,summary,body,tags, tokenize='unicode61');`)
	if err != nil {
		return err
	}
	spaceRows, err := s.DB.Query(`PRAGMA table_info(spaces)`)
	if err != nil {
		return err
	}
	hasArchivedAt := false
	for spaceRows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def sql.NullString
		if err := spaceRows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			spaceRows.Close()
			return err
		}
		if name == "archived_at" {
			hasArchivedAt = true
		}
	}
	if err := spaceRows.Err(); err != nil {
		spaceRows.Close()
		return err
	}
	if err := spaceRows.Close(); err != nil {
		return err
	}
	if !hasArchivedAt {
		if _, err := s.DB.Exec(`ALTER TABLE spaces ADD COLUMN archived_at TEXT`); err != nil {
			return err
		}
	}
	rows, err := s.DB.Query(`PRAGMA table_info(articles)`)
	if err != nil {
		return err
	}
	hasArticleDeletedAt := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "deleted_at" {
			hasArticleDeletedAt = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasArticleDeletedAt {
		if _, err = s.DB.Exec(`ALTER TABLE articles ADD COLUMN deleted_at TEXT`); err != nil {
			return err
		}
	}
	rows, err = s.DB.Query(`PRAGMA table_info(attachments)`)
	if err != nil {
		return err
	}
	hasDeletedAt := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "deleted_at" {
			hasDeletedAt = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasDeletedAt {
		_, err = s.DB.Exec(`ALTER TABLE attachments ADD COLUMN deleted_at TEXT`)
	}
	if err == nil {
		err = s.ensureSQLiteProjectColumn()
	}
	if err == nil {
		err = s.migrateArtifactsSQLite()
	}
	if err == nil {
		err = s.migrateSemanticSQLite()
	}
	return err
}
func (s *Server) ensureSQLiteProjectColumn() error {
	rows, err := s.DB.Query("PRAGMA table_info(articles)")
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def sql.NullString
		if err = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		has = has || name == "project_id"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !has {
		_, err = s.DB.Exec("ALTER TABLE articles ADD COLUMN project_id TEXT NOT NULL DEFAULT ''")
	}
	return err
}

func (s *Server) seed() error {
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM spaces`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	spaces := []Space{{ID: "general", Name: "Общее", Description: "Общие правила и знания"}, {ID: "genealogy", Name: "Родословная", Description: "Исследования семейной истории"}, {ID: "engineering", Name: "Инженерия", Description: "Инженерные заметки и решения"}}
	for _, v := range spaces {
		if _, err = tx.Exec(`INSERT INTO spaces(id,name,description) VALUES(?,?,?)`, v.ID, v.Name, v.Description); err != nil {
			return err
		}
	}
	type seedDoc struct {
		id, space, title, summary, body string
		pinned                          bool
	}
	docs := []seedDoc{
		{"start-here", "general", "Начните здесь", "Как устроена эта база знаний и как вносить новые материалы.", "# Начните здесь\n\nЭто локальная база знаний. Выбирайте раздел слева, ищите по тексту и тегам, а для каждой статьи сохраняется история версий.\n\n## Перед добавлением\n\nПроверьте поиск и ссылки. Добавляйте выводы вместе с источниками и датой проверки. Сомнения помечайте явно.\n\n## Полезные инструкции\n\n- [Правила для агентов](/?article=agent-rules)\n- [Как записывать знания](/?article=writing-knowledge)", true},
		{"agent-rules", "general", "Правила для агентов", "Общие правила для AI-агентов: следовать указаниям владельца, проверять результаты, публиковать только с разрешения.", "# Правила для агентов\n\nСледуйте указаниям владельца и правилам проекта. Если задача или ограничение неясны, уточните их, а не угадывайте. Если нужной возможности нет, сообщите об ограничении и продолжайте доступным способом.\n\nПроверяйте результат сами: читайте исходный код, запускайте уместные сборки и тесты, сверяйтесь с первоисточниками. Чужой отчёт и собственное предположение не заменяют проверку. Сообщайте, что именно проверено и с каким результатом, и не утверждайте того, чего не делали.\n\nНе меняйте файлы и каталоги вне порученной области и не выполняйте git-команды без прямого поручения. Публикация, отправка сообщений, push и другие внешние действия допустимы только после явного разрешения владельца.\n\nВладелец задаёт цели и принимает архитектурные решения. Дополнительные правила проекта записывайте отдельными закреплёнными статьями.", true},
		{"writing-knowledge", "general", "Как записывать знания", "Фиксируйте источники, уверенность и безуспешные проверки.", "# Как записывать знания\n\nПишите короткое резюме, которое помогает найти материал. В основном тексте отделяйте подтверждённые факты от гипотез и указывайте источник, дату доступа и точное место в источнике.\n\nЕсли поиск или проверка ничего не дали, запишите запрос, где искали и когда. Это помогает не повторять бесполезную работу. Указывайте условия, при которых вывод может устареть.\n\nДля спорных утверждений добавляйте ссылки и сохраняйте исходные данные. Не придумывайте результаты ради заполнения шаблона.", true},
		{"research-template", "genealogy", "Шаблон исследования", "Пустой шаблон; замените подсказки данными из проверяемых источников.", "# Вопрос\n\nОпишите исследовательский вопрос.\n\n## Что известно\n\nДобавьте только подтверждённые сведения с источниками.\n\n## Гипотезы\n\nПометьте предположения и укажите, как их проверить.\n\n## Источники и поиски\n\nЗаписывайте архив, опись, дело, лист, дату доступа и безуспешные запросы.", false},
		{"engineering-template", "engineering", "Шаблон инженерной заметки", "Пустой шаблон решения; пример не содержит вымышленных результатов.", "# Контекст\n\nОпишите задачу и ограничения.\n\n## Решение\n\nЗапишите проверяемое решение и его основания.\n\n## Проверка\n\nУкажите выполненные проверки и результат.\n\n## Риски и открытые вопросы\n\nПеречислите то, что ещё требует проверки.", false},
	}
	for _, d := range docs {
		a := Article{ID: d.id, SpaceID: d.space, Title: d.title, Summary: d.summary, Body: d.body, Kind: "instruction", Status: "active", Pinned: d.pinned, Tags: []string{}, Properties: map[string]string{}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "system"}
		if err := insertArticle(tx, a, "Seed guidance"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertArticle(tx *transaction, a Article, message string) error {
	_, err := tx.Exec(`INSERT INTO articles(`+articleColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.SpaceID, a.ParentID, a.ProjectID, a.Title, a.Summary, a.Body, a.Kind, a.Status, boolInt(a.Pinned), encode(a.Tags), encode(a.Properties), a.Revision, a.CreatedAt, a.UpdatedAt, a.Author, nullString(a.DeletedAt))
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)`, a.ID, a.Revision, a.Author, message, a.CreatedAt, encode(a)); err != nil {
		return err
	}
	return indexArticle(tx, a)
}
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// indexArticle replaces the search row by delete and insert in both databases.
// In PostgreSQL an upsert would need UPDATE on article_fts, which the MCP scope
// policies deny to write keys; they permit exactly insert and delete.
func indexArticle(tx *transaction, a Article) error {
	if _, e := tx.Exec(`DELETE FROM article_fts WHERE article_id=?`, a.ID); e != nil {
		return e
	}
	_, e := tx.Exec(`INSERT INTO article_fts(article_id,title,summary,body,tags) VALUES(?,?,?,?,?)`, a.ID, a.Title, a.Summary, a.Body, join(a.Tags, " "))
	return e
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func join(a []string, sep string) string {
	b := ""
	for i, v := range a {
		if i > 0 {
			b += sep
		}
		b += v
	}
	return b
}
