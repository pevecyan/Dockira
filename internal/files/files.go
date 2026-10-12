// Package files edits the files of a stack's folder: compose files and .env (validated by compose
// itself before anything is written) and any other file (syntax-checked when it's YAML or JSON).
// Paths never leave the stack's folder (symlinks resolved), writes are atomic and keep mode and
// owner, a file that changed since the editor loaded it is not overwritten, and every version
// is kept in SQLite (edits, restores, deletions and outside changes; all but compose files
// encrypted, since config files hold keys).
package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"docker-manager/internal/dotenv"
	"docker-manager/internal/secret"
	"docker-manager/internal/stacks"
	"docker-manager/internal/store"
)

const (
	EnvName = ".env"
	MaxSize = 1 << 20
	keep    = 50 // revisions kept per file
)

type Validator interface {
	Validate(ctx context.Context, dir string, files []string) error
	ValidateEnv(ctx context.Context, dir string, files []string, envFile string) error
}

type File struct {
	Name    string    `json:"name"`
	Kind    string    `json:"kind" enum:"compose,override,env,file"`
	Exists  bool      `json:"exists"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time,omitzero"`
	SHA256  string    `json:"sha256,omitempty"`
}

type Revision struct {
	ID        int64     `json:"id"`
	StackID   string    `json:"stack_id"`
	File      string    `json:"file"`
	SHA256    string    `json:"sha256"`
	Reason    string    `json:"reason" enum:"baseline,edit,restore,external,delete"`
	IP        string    `json:"ip,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Size      int       `json:"size"`
	Content   string    `json:"content,omitempty"`
}

// ValidationError points at the line compose complained about when it can tell.
type ValidationError struct {
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

func (e *ValidationError) Error() string { return e.Message }

var (
	ErrNotAllowed = errors.New("this file can't be opened here")
	ErrBinary     = errors.New("a binary file: download it instead")
	ErrIsDir      = errors.New("that is a folder")
	ErrConflict   = errors.New("the file changed since you opened it")
	ErrTooLarge   = fmt.Errorf("files over %d KiB can't be edited here", MaxSize>>10)
)

type Manager struct {
	DB        *sql.DB
	Validator Validator
	Box       *secret.Box // encrypts stored .env revisions (they hold secrets)
}

const encPrefix = "enc:v1:"

// Sensitive: anything but compose files may hold secrets (reading needs a password confirmation).
func Sensitive(rel string) bool {
	c, _ := clean(rel)
	return !isComposeName(c)
}

// isComposeName: compose files are stored readable; everything else may hold secrets.
func isComposeName(name string) bool {
	if strings.Contains(name, "/") {
		return false
	}
	for _, n := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
		"compose.override.yaml", "compose.override.yml", "docker-compose.override.yaml", "docker-compose.override.yml"} {
		if name == n {
			return true
		}
	}
	return false
}

func (m *Manager) seal(name, content string) string {
	if isComposeName(name) || m.Box == nil {
		return content
	}
	b, err := m.Box.Seal([]byte(content))
	if err != nil {
		return ""
	}
	return encPrefix + base64.StdEncoding.EncodeToString(b)
}

func (m *Manager) open(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	if m.Box == nil {
		return "", errors.New("encrypted revision: no key")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", err
	}
	plain, err := m.Box.Open(b)
	return string(plain), err
}

// List returns the stack's editable files: its compose files and .env (which may not exist yet).
func List(st stacks.Stack) []File {
	var out []File
	for i, p := range st.Files {
		kind := "compose"
		if i > 0 {
			kind = "override"
		}
		out = append(out, describe(p, kind))
	}
	if st.Dir != "" {
		out = append(out, describe(filepath.Join(st.Dir, EnvName), "env"))
	}
	return out
}

func describe(p, kind string) File {
	f := File{Name: filepath.Base(p), Kind: kind}
	if st, err := os.Stat(p); err == nil {
		f.Exists, f.Size, f.ModTime = true, st.Size(), st.ModTime()
		if b, err := os.ReadFile(p); err == nil {
			f.SHA256 = Hash(string(b))
		}
	}
	return f
}

func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// clean turns a user-supplied path into a clean one relative to the stack folder ("" = the folder).
func clean(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", ErrNotAllowed
	}
	c := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
	return c, nil
}

// protected keeps docker-manager's own secrets out of reach of its UI.
func protected(st stacks.Stack, rel string) bool {
	return st.Self && (rel == "config.yaml" || rel == "data" || strings.HasPrefix(rel, "data/"))
}

// resolve maps a path relative to the stack folder to a real path inside it (following
// symlinks only while they stay inside), for existing and new files alike.
func resolve(st stacks.Stack, rel string) (string, string, error) {
	if st.Orphaned || st.Dir == "" {
		return "", "", errors.New("the stack's folder is missing")
	}
	rel, err := clean(rel)
	if err != nil {
		return "", "", err
	}
	if protected(st, rel) {
		return "", "", ErrNotAllowed
	}
	dir, err := filepath.EvalSymlinks(st.Dir)
	if err != nil {
		return "", "", err
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	// the deepest existing part of the path must resolve inside the folder
	probe := p
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if real != dir && !strings.HasPrefix(real, dir+string(filepath.Separator)) {
				return "", "", fmt.Errorf("%s points outside the stack folder", rel)
			}
			if probe == p {
				p = real
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || probe == dir {
			return "", "", err
		}
		probe = filepath.Dir(probe)
	}
	return p, rel, nil
}

// path resolves a file of the stack folder and tells what kind it is.
func path(st stacks.Stack, name string) (string, string, error) {
	p, rel, err := resolve(st, name)
	if err != nil {
		return "", "", err
	}
	if rel == "" {
		return "", "", ErrIsDir
	}
	kind := "file"
	if rel == EnvName {
		kind = "env"
	}
	for i, f := range st.Files {
		if filepath.Base(f) == rel {
			kind = "compose"
			if i > 0 {
				kind = "override"
			}
		}
	}
	return p, kind, nil
}

// Read returns a file's content and hash. A first read records a baseline revision; a file
// that differs from its last revision was changed elsewhere, which is recorded too.
func (m *Manager) Read(ctx context.Context, st stacks.Stack, name string) (string, string, error) {
	p, _, err := path(st, name)
	if err != nil {
		return "", "", err
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return "", "", ErrIsDir
	} else if err == nil && fi.Size() > MaxSize {
		return "", "", ErrTooLarge
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil // not there yet (a new .env or file): empty
	}
	if err != nil {
		return "", "", err
	}
	if IsBinary(b) {
		return "", "", ErrBinary
	}
	content := string(b)
	sum := Hash(content)
	rel, _ := clean(name)
	m.track(ctx, st.ID, rel, content, sum, "")
	return content, sum, nil
}

// IsBinary: NUL bytes or invalid UTF-8 near the start.
func IsBinary(b []byte) bool {
	head := b[:min(len(b), 8000)]
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	for len(head) > 0 {
		r, size := utf8.DecodeRune(head)
		if r == utf8.RuneError && size == 1 && len(head) >= utf8.UTFMax {
			return true
		}
		head = head[size:]
	}
	return false
}

// track records the current content when it differs from the last revision and returns the
// reason recorded ("" when nothing was).
func (m *Manager) track(ctx context.Context, stackID, name, content, sum, ip string) string {
	last, err := m.last(ctx, stackID, name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		m.record(ctx, stackID, name, content, sum, "baseline", ip)
		return "baseline"
	case err == nil && last != sum:
		m.record(ctx, stackID, name, content, sum, "external", ip)
		return "external"
	}
	return ""
}

func (m *Manager) last(ctx context.Context, stackID, name string) (string, error) {
	var sum string
	err := m.DB.QueryRowContext(ctx, `SELECT sha256 FROM file_revisions WHERE stack_id = ? AND file_name = ? ORDER BY id DESC LIMIT 1`,
		stackID, name).Scan(&sum)
	return sum, err
}

func (m *Manager) record(ctx context.Context, stackID, name, content, sum, reason, ip string) int64 {
	res, err := m.DB.ExecContext(ctx, `INSERT INTO file_revisions (stack_id, file_name, sha256, content, reason, ip) VALUES (?, ?, ?, ?, ?, ?)`,
		stackID, name, sum, m.seal(name, content), reason, ip)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	_, _ = m.DB.ExecContext(ctx, `DELETE FROM file_revisions WHERE stack_id = ? AND file_name = ? AND id NOT IN
		(SELECT id FROM file_revisions WHERE stack_id = ? AND file_name = ? ORDER BY id DESC LIMIT ?)`, stackID, name, stackID, name, keep)
	return id
}

// Validate checks content as if it replaced the file: written to a temporary file next to the
// real one (so relative paths and .env resolve the same) and given to `compose config`.
func (m *Manager) Validate(ctx context.Context, st stacks.Stack, name, content string) error {
	p, kind, err := path(st, name)
	if err != nil {
		return err
	}
	if len(content) > MaxSize {
		return ErrTooLarge
	}
	if kind == "file" {
		return checkSyntax(name, content)
	}
	if kind == "env" {
		if _, bad := dotenv.Parse(content); len(bad) > 0 {
			return &ValidationError{Message: fmt.Sprintf("line %d is not KEY=value", bad[0]), Line: bad[0]}
		}
	}
	tmp, err := tempFile(filepath.Dir(p), name, content)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if kind == "env" {
		err = m.Validator.ValidateEnv(ctx, st.Dir, st.Files, tmp)
	} else {
		files := append([]string(nil), st.Files...)
		for i, f := range files {
			if filepath.Base(f) == name {
				files[i] = tmp
			}
		}
		err = m.Validator.Validate(ctx, st.Dir, files)
	}
	if err != nil {
		return parseError(strings.ReplaceAll(err.Error(), filepath.Base(tmp), name))
	}
	return nil
}

// checkSyntax catches broken YAML and JSON before they're written; other files are taken as they are.
func checkSyntax(name, content string) error {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		var v any
		if err := json.Unmarshal([]byte(content), &v); err != nil {
			e := &ValidationError{Message: "JSON: " + err.Error()}
			var se *json.SyntaxError
			if errors.As(err, &se) {
				e.Line = strings.Count(content[:min(int(se.Offset), len(content))], "\n") + 1
			}
			return e
		}
	case ".yaml", ".yml":
		dec := yaml.NewDecoder(strings.NewReader(content))
		for {
			var n yaml.Node
			if err := dec.Decode(&n); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return parseError("YAML: " + err.Error())
			}
		}
	}
	return nil
}

var lineRe = regexp.MustCompile(`(?:line (\d+)(?:, column (\d+))?|L(\d+)\.C(\d+))`)

func parseError(msg string) *ValidationError {
	e := &ValidationError{Message: msg}
	if m := lineRe.FindStringSubmatch(msg); m != nil {
		e.Line, _ = strconv.Atoi(m[1] + m[3])
		e.Column, _ = strconv.Atoi(m[2] + m[4])
	}
	return e
}

func tempFile(dir, name, content string) (string, error) {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	p := filepath.Join(dir, ".dm-"+hex.EncodeToString(b)+"-"+filepath.Base(name))
	return p, os.WriteFile(p, []byte(content), 0o600)
}

// Save validates and writes a file. base is the hash the editor loaded ("" for a new file);
// if the file changed since, ErrConflict and nothing is written.
func (m *Manager) Save(ctx context.Context, st stacks.Stack, name, content, base, reason, ip string) (Revision, error) {
	p, _, err := path(st, name)
	if err != nil {
		return Revision{}, err
	}
	name, _ = clean(name)
	cur, curSum, err := m.Read(ctx, st, name)
	if err != nil {
		return Revision{}, err
	}
	if curSum != base {
		return Revision{}, ErrConflict
	}
	sum := Hash(content)
	if sum == curSum && cur != "" {
		r, err := m.latest(ctx, st.ID, name)
		return r, err // nothing changed
	}
	if err := m.Validate(ctx, st, name, content); err != nil {
		return Revision{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { // a new file in a new folder
		return Revision{}, err
	}
	if err := writeAtomic(p, []byte(content)); err != nil {
		return Revision{}, err
	}
	id := m.record(ctx, st.ID, name, content, sum, reason, ip)
	return Revision{ID: id, StackID: st.ID, File: name, SHA256: sum, Reason: reason, IP: ip, CreatedAt: time.Now(), Size: len(content)}, nil
}

// writeAtomic replaces p via a temporary file in the same folder, keeping mode and owner.
func writeAtomic(p string, content []byte) error {
	mode := fs.FileMode(0o644)
	uid, gid := -1, -1
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
		if s, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(s.Uid), int(s.Gid)
		}
	} else if filepath.Base(p) == EnvName {
		mode = 0o600 // a new .env holds secrets
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".dm-save-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if uid >= 0 {
		_ = os.Chown(tmp.Name(), uid, gid) // only works as root or the owner; fine either way
	}
	return os.Rename(tmp.Name(), p)
}

// Revisions lists a stack's revisions (optionally of one file), newest first, without content.
func (m *Manager) Revisions(ctx context.Context, stackID, name string, limit int) ([]Revision, error) {
	q := `SELECT id, stack_id, file_name, sha256, reason, ip, created_at, 0 FROM file_revisions WHERE stack_id = ?`
	args := []any{stackID}
	if name != "" {
		q += ` AND file_name = ?`
		args = append(args, name)
	}
	rows, err := m.DB.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		var ts string
		if err := rows.Scan(&r.ID, &r.StackID, &r.File, &r.SHA256, &r.Reason, &r.IP, &ts, &r.Size); err != nil {
			return nil, err
		}
		r.CreatedAt = store.ParseTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revision returns one revision with its content.
func (m *Manager) Revision(ctx context.Context, stackID string, id int64) (Revision, error) {
	var r Revision
	var ts string
	err := m.DB.QueryRowContext(ctx, `SELECT id, stack_id, file_name, sha256, reason, ip, created_at, content FROM file_revisions WHERE id = ? AND stack_id = ?`,
		id, stackID).Scan(&r.ID, &r.StackID, &r.File, &r.SHA256, &r.Reason, &r.IP, &ts, &r.Content)
	r.CreatedAt = store.ParseTime(ts)
	if err == nil {
		r.Content, err = m.open(r.Content)
	}
	r.Size = len(r.Content)
	return r, err
}

func (m *Manager) latest(ctx context.Context, stackID, name string) (Revision, error) {
	var id int64
	if err := m.DB.QueryRowContext(ctx, `SELECT id FROM file_revisions WHERE stack_id = ? AND file_name = ? ORDER BY id DESC LIMIT 1`,
		stackID, name).Scan(&id); err != nil {
		return Revision{}, err
	}
	r, err := m.Revision(ctx, stackID, id)
	r.Content = ""
	return r, err
}

type Change struct {
	StackID string
	File    string
}

// TrackAll records outside changes to every stack's files (called after scans) and returns them.
func (m *Manager) TrackAll(ctx context.Context, all []stacks.Stack) []Change {
	var out []Change
	for _, st := range all {
		if st.Orphaned {
			continue
		}
		for _, f := range List(st) {
			if !f.Exists || f.Size > MaxSize {
				continue
			}
			p, _, err := path(st, f.Name)
			if err != nil {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if m.track(ctx, st.ID, f.Name, string(b), Hash(string(b)), "") == "external" {
				out = append(out, Change{st.ID, f.Name})
			}
		}
	}
	return out
}

// Entry is one item of a folder listing.
type Entry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path" doc:"Relative to the stack folder"`
	Type     string    `json:"type" enum:"dir,file,symlink"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Mode     string    `json:"mode"`
	Target   string    `json:"target,omitempty" doc:"Symlink target"`
	Outside  bool      `json:"outside,omitempty" doc:"A symlink leaving the stack folder (not opened)"`
	Locked   bool      `json:"locked,omitempty" doc:"Not available here (Dockira's own secrets)"`
	Readable bool      `json:"readable"`
}

type Listing struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated" doc:"More than 2000 entries: only the first are listed"`
}

const maxEntries = 2000

// Tree lists one folder of the stack (folders first, then by name).
func Tree(st stacks.Stack, rel string) (Listing, error) {
	p, rel, err := resolve(st, rel)
	if err != nil {
		return Listing{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return Listing{}, err
	}
	if !fi.IsDir() {
		return Listing{}, errors.New("not a folder")
	}
	des, err := os.ReadDir(p)
	if err != nil {
		return Listing{}, err
	}
	root, _ := filepath.EvalSymlinks(st.Dir)
	out := Listing{Path: rel, Entries: []Entry{}}
	for _, de := range des {
		if len(out.Entries) >= maxEntries {
			out.Truncated = true
			break
		}
		name := de.Name()
		if strings.HasPrefix(name, ".dm-") { // our own temporary files
			continue
		}
		ep := strings.TrimPrefix(rel+"/"+name, "/")
		e := Entry{Name: name, Path: ep, Type: "file", Readable: true}
		info, err := de.Info()
		if err != nil {
			continue
		}
		e.Size, e.ModTime, e.Mode = info.Size(), info.ModTime(), info.Mode().String()
		if info.Mode()&fs.ModeSymlink != 0 {
			e.Type = "symlink"
			e.Target, _ = os.Readlink(filepath.Join(p, name))
			real, err := filepath.EvalSymlinks(filepath.Join(p, name))
			e.Outside = err != nil || (real != root && !strings.HasPrefix(real, root+string(filepath.Separator)))
			e.Readable = !e.Outside
			if !e.Outside {
				if ti, err := os.Stat(real); err == nil && ti.IsDir() {
					e.Type = "dir"
				}
			}
		} else if de.IsDir() {
			e.Type = "dir"
		}
		if protected(st, ep) {
			e.Locked, e.Readable = true, false
		}
		out.Entries = append(out.Entries, e)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out, nil
}

// Mkdir creates a folder (and missing parents) inside the stack folder.
func Mkdir(st stacks.Stack, rel string) error {
	p, rel, err := resolve(st, rel)
	if err != nil {
		return err
	}
	if rel == "" {
		return errors.New("name the folder")
	}
	return os.MkdirAll(p, 0o755)
}

// Delete removes a file (its last content is kept as a "delete" revision, so it can be
// restored from History) or an empty folder.
func (m *Manager) Delete(ctx context.Context, st stacks.Stack, rel, ip string) error {
	p, rel, err := resolve(st, rel)
	if err != nil {
		return err
	}
	if rel == "" {
		return errors.New("the stack folder itself can't be deleted here")
	}
	for _, f := range st.Files {
		if filepath.Base(f) == rel {
			return errors.New("the compose file can't be deleted here")
		}
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.Remove(p); err != nil {
			return errors.New("only empty folders can be deleted")
		}
		return nil
	}
	if fi.Mode().IsRegular() && fi.Size() <= MaxSize {
		if b, err := os.ReadFile(p); err == nil && !IsBinary(b) {
			m.record(ctx, st.ID, rel, string(b), Hash(string(b)), "delete", ip)
		}
	}
	return os.Remove(p)
}

// Upload writes a file from the browser (any content, atomically). Text files get a revision.
func (m *Manager) Upload(ctx context.Context, st stacks.Stack, rel string, content []byte, ip string) error {
	p, kind, err := path(st, rel)
	if err != nil {
		return err
	}
	rel, _ = clean(rel)
	if kind != "file" {
		return errors.New("compose files and .env are edited in their own tabs")
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return ErrIsDir
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(p, content); err != nil {
		return err
	}
	if len(content) <= MaxSize && !IsBinary(content) {
		m.record(ctx, st.ID, rel, string(content), Hash(string(content)), "edit", ip)
	}
	return nil
}

// Open opens a file of the stack folder for download.
func Open(st stacks.Stack, rel string) (*os.File, fs.FileInfo, error) {
	p, _, err := path(st, rel)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		f.Close()
		return nil, nil, ErrIsDir
	}
	return f, fi, nil
}

// Stat describes one file (for the viewer: size, binary, too large).
func Stat(st stacks.Stack, rel string) (Entry, error) {
	p, rel, err := resolve(st, rel)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Name: filepath.Base(rel), Path: rel, Type: "file", Size: fi.Size(), ModTime: fi.ModTime(), Mode: fi.Mode().String(), Readable: true}
	if fi.IsDir() {
		e.Type = "dir"
	}
	return e, nil
}
