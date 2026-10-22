package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Artifact limits (docs/ARTIFACTS.md, "Model").
const (
	artifactMaxFiles        = 50
	artifactMaxFileBytes    = 5 << 20
	artifactMaxRevisionSize = 20 << 20
	artifactMaxPathBytes    = 200
	artifactMaxPathDepth    = 4
	artifactMaxProjection   = 100 << 10
	artifactFileProjection  = 16 << 10
	artifactMaxTitle        = 300
	artifactMaxDescription  = 2000
	artifactMaxMessage      = 500
	// artifactMaxBody bounds a JSON request: 20 MB of files in base64 plus slack.
	artifactMaxBody = 28 << 20
)

var artifactPathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// artifactMIME is the allow-list: extension to the only MIME type served for it.
var artifactMIME = map[string]string{
	"html": "text/html", "css": "text/css", "js": "text/javascript", "mjs": "text/javascript",
	"json": "application/json", "csv": "text/csv", "txt": "text/plain", "md": "text/markdown",
	"svg": "image/svg+xml", "png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg",
	"gif": "image/gif", "webp": "image/webp",
}

// artifactContentType is the Content-Type header value for a manifest MIME.
func artifactContentType(mime string) string {
	if strings.HasPrefix(mime, "text/") || mime == "application/json" {
		return mime + "; charset=utf-8"
	}
	return mime
}

// artifactFile is one manifest entry.
type artifactFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime"`
}

type artifactFileOut struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime"`
}

func publicManifest(files []artifactFile) []artifactFileOut {
	out := make([]artifactFileOut, 0, len(files))
	for _, f := range files {
		out = append(out, artifactFileOut(f))
	}
	return out
}

// artifactError carries an HTTP status for a client-facing failure.
type artifactError struct {
	Code int
	Msg  string
}

func (e *artifactError) Error() string { return e.Msg }

func artErr(code int, format string, args ...any) *artifactError {
	return &artifactError{code, fmt.Sprintf(format, args...)}
}

// validArtifactPath reports whether p is an acceptable file path and returns
// the MIME type of its extension. It is deliberately stricter than the pattern:
// any ".." is refused.
func validArtifactPath(p string) (string, error) {
	if p == "" || len(p) > artifactMaxPathBytes {
		return "", artErr(400, "invalid file path %q: empty or longer than %d bytes", clip(p, 40), artifactMaxPathBytes)
	}
	if !artifactPathPattern.MatchString(p) || strings.Contains(p, "..") {
		return "", artErr(400, "invalid file path %q", clip(p, 80))
	}
	if strings.Count(p, "/")+1 > artifactMaxPathDepth {
		return "", artErr(400, "file path %q is deeper than %d levels", clip(p, 80), artifactMaxPathDepth)
	}
	base := p[strings.LastIndexByte(p, '/')+1:]
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 || dot == len(base)-1 {
		return "", artErr(400, "file %q needs an allowed extension", clip(p, 80))
	}
	mime, ok := artifactMIME[strings.ToLower(base[dot+1:])]
	if !ok {
		return "", artErr(400, "extension of %q is not allowed", clip(p, 80))
	}
	return mime, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// artifactFileIn is a file in a create/update request. Delete removes the path.
type artifactFileIn struct {
	Path     string  `json:"path"`
	Content  *string `json:"content"`
	Encoding string  `json:"encoding"`
	Delete   bool    `json:"delete"`
}

type artifactBlob struct {
	File artifactFile
	Data []byte
	Text string // text projection of the file
}

// parseArtifactFiles validates request entries and returns the files to store
// and the paths to delete. Every path may appear once.
func parseArtifactFiles(in []artifactFileIn, allowDelete bool) ([]artifactBlob, []string, error) {
	if len(in) > artifactMaxFiles*2 {
		return nil, nil, artErr(400, "too many file entries")
	}
	var blobs []artifactBlob
	var deletes []string
	seen := map[string]bool{}
	for _, f := range in {
		mime, err := validArtifactPath(f.Path)
		if err != nil {
			return nil, nil, err
		}
		key := strings.ToLower(f.Path)
		if seen[key] {
			return nil, nil, artErr(400, "file %q is listed twice", f.Path)
		}
		seen[key] = true
		if f.Delete {
			if !allowDelete {
				return nil, nil, artErr(400, "delete is only valid when updating")
			}
			if f.Content != nil {
				return nil, nil, artErr(400, "file %q: delete cannot carry content", f.Path)
			}
			deletes = append(deletes, f.Path)
			continue
		}
		if f.Content == nil {
			return nil, nil, artErr(400, "file %q: content is required", f.Path)
		}
		var data []byte
		switch f.Encoding {
		case "", "utf8":
			if len(*f.Content) > artifactMaxFileBytes {
				return nil, nil, artErr(413, "file %q exceeds %d MB", f.Path, artifactMaxFileBytes>>20)
			}
			if !utf8.ValidString(*f.Content) {
				return nil, nil, artErr(400, "file %q: content is not valid UTF-8", f.Path)
			}
			data = []byte(*f.Content)
		case "base64":
			if base64.StdEncoding.DecodedLen(len(*f.Content)) > artifactMaxFileBytes+3 {
				return nil, nil, artErr(413, "file %q exceeds %d MB", f.Path, artifactMaxFileBytes>>20)
			}
			var err error
			if data, err = base64.StdEncoding.DecodeString(*f.Content); err != nil {
				return nil, nil, artErr(400, "file %q: invalid base64", f.Path)
			}
		default:
			return nil, nil, artErr(400, "file %q: encoding must be utf8 or base64", f.Path)
		}
		if len(data) > artifactMaxFileBytes {
			return nil, nil, artErr(413, "file %q exceeds %d MB", f.Path, artifactMaxFileBytes>>20)
		}
		sum := sha256.Sum256(data)
		blobs = append(blobs, artifactBlob{File: artifactFile{Path: f.Path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), MIME: mime}, Data: data, Text: artifactProjection(f.Path, data)})
	}
	return blobs, deletes, nil
}

// mergeArtifactFiles applies changes to the current manifest and enforces the
// per-revision limits, index.html and the file/directory exclusion.
func mergeArtifactFiles(current []artifactFile, blobs []artifactBlob, deletes []string) ([]artifactFile, error) {
	byPath := map[string]artifactFile{}
	for _, f := range current {
		byPath[f.Path] = f
	}
	for _, d := range deletes {
		if _, ok := byPath[d]; !ok {
			return nil, artErr(400, "cannot delete %q: no such file", d)
		}
		delete(byPath, d)
	}
	for _, b := range blobs {
		byPath[b.File.Path] = b.File
	}
	out := make([]artifactFile, 0, len(byPath))
	var total int64
	for _, f := range byPath {
		out = append(out, f)
		total += f.Size
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > artifactMaxFiles {
		return nil, artErr(413, "an artifact has at most %d files", artifactMaxFiles)
	}
	if total > artifactMaxRevisionSize {
		return nil, artErr(413, "a revision is at most %d MB", artifactMaxRevisionSize>>20)
	}
	if _, ok := byPath["index.html"]; !ok {
		return nil, artErr(400, "index.html is required")
	}
	for _, f := range out {
		for i := 0; i < len(f.Path); i++ {
			if f.Path[i] == '/' {
				if _, clash := byPath[f.Path[:i]]; clash {
					return nil, artErr(400, "%q is both a file and a directory", f.Path[:i])
				}
			}
		}
	}
	return out, nil
}

// ---- text projection ----

var (
	htmlDropBlocks = regexp.MustCompile(`(?is)<(script|style|noscript|template|head)\b.*?</(script|style|noscript|template|head)\s*>`)
	htmlComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlTitle      = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
	spaceRun       = regexp.MustCompile(`\s+`)
)

func collapseSpace(s string) string { return strings.TrimSpace(spaceRun.ReplaceAllString(s, " ")) }

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// artifactProjection is the searchable visible text of one file: the text of
// HTML/SVG/Markdown/plain files, the header and first rows of CSV, and the keys
// and string values of JSON. Code and images contribute nothing.
func artifactProjection(path string, data []byte) string {
	ext := strings.ToLower(path[strings.LastIndexByte(path, '.')+1:])
	var text string
	switch ext {
	case "html", "svg":
		src := string(data)
		title := ""
		if m := htmlTitle.FindStringSubmatch(src); m != nil {
			title = m[1]
		}
		src = htmlComment.ReplaceAllString(src, " ")
		src = htmlDropBlocks.ReplaceAllString(src, " ")
		src = htmlTag.ReplaceAllString(src, " ")
		text = collapseSpace(html.UnescapeString(title + " " + src))
	case "md", "txt":
		text = collapseSpace(string(data))
	case "csv":
		lines := strings.SplitN(string(data), "\n", 22)
		if len(lines) > 21 {
			lines = lines[:21]
		}
		text = collapseSpace(strings.Join(lines, " "))
	case "json":
		text = jsonProjection(data)
	default:
		return ""
	}
	return clipBytes(text, artifactFileProjection)
}

func jsonProjection(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	for n := 0; n < 600 && b.Len() < 4096; n++ {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if s, ok := tok.(string); ok {
			b.WriteString(s)
			b.WriteByte(' ')
		}
	}
	return collapseSpace(b.String())
}

// artifactProjectionSet is the per-file projection of the current revision
// (path to text), stored as JSON in artifacts.projection.
type artifactProjectionSet map[string]string

func parseProjectionSet(raw string) artifactProjectionSet {
	set := artifactProjectionSet{}
	_ = json.Unmarshal([]byte(raw), &set)
	if set == nil {
		set = artifactProjectionSet{}
	}
	return set
}

// next derives the projection of a new revision from the previous one: deleted
// and rewritten files drop their old text, new files bring theirs.
func (p artifactProjectionSet) next(files []artifactFile, blobs []artifactBlob, deletes []string) artifactProjectionSet {
	out := artifactProjectionSet{}
	for k, v := range p {
		out[k] = v
	}
	for _, d := range deletes {
		delete(out, d)
	}
	for _, b := range blobs {
		if b.Text != "" {
			out[b.File.Path] = b.Text
		} else {
			delete(out, b.File.Path)
		}
	}
	keep := map[string]bool{}
	for _, f := range files {
		keep[f.Path] = true
	}
	for k := range out {
		if !keep[k] {
			delete(out, k)
		}
	}
	return out
}

// artifactSearchText is the lower-cased text searched by q: title, description
// and the projection of the files, at most 100 KB. Lower-casing happens in Go so
// Cyrillic matching works identically in SQLite and PostgreSQL.
func artifactSearchText(title, description string, files []artifactFile, proj artifactProjectionSet) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteByte('\n')
	b.WriteString(description)
	for _, f := range files {
		if t := proj[f.Path]; t != "" {
			b.WriteByte('\n')
			b.WriteString(t)
		}
	}
	return clipBytes(strings.ToLower(b.String()), artifactMaxProjection)
}

func artifactBlobKey(sha string) string { return "art-" + sha }
