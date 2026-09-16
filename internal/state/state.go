package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

const SchemaVersion = 1

type Repo struct {
	RelPath           string `json:"rel_path"`
	AbsPath           string `json:"abs_path"`
	StoreID           string `json:"store_id"`
	Branch            string `json:"branch"`
	BaseSHA           string `json:"base_sha"`
	BaseRef           string `json:"base_ref"`
	WorktreePath      string `json:"worktree_path"`
	StoreWorktreeName string `json:"store_worktree_name"`
	BasePinRef        string `json:"base_pin_ref"`
}

type LinkSlot struct {
	RelPath string `json:"rel_path"`
	Target  string `json:"target"`
	Type    string `json:"type"`
	Hash    string `json:"hash,omitempty"`
}

type Task struct {
	SchemaVersion      int               `json:"schema_version"`
	Name               string            `json:"name"`
	Container          string            `json:"container"`
	Workspace          string            `json:"workspace"`
	WorkspaceSpellings []string          `json:"workspace_spellings"`
	BaseEpoch          time.Time         `json:"base_epoch"`
	Repos              []Repo            `json:"repos"`
	Links              []LinkSlot        `json:"links"`
	PerimeterCoverage  []string          `json:"perimeter_coverage,omitempty"`
	PerimeterHashes    map[string]string `json:"perimeter_hashes,omitempty"`
}

func path(dir, name string) string { return filepath.Join(dir, name+".json") }

// Save writes a task to a temporary file in the same directory, then renames it
// for atomic writes. This ensures the final file is either complete or not present.
func Save(dir string, t Task) error {
	if t.SchemaVersion == 0 {
		t.SchemaVersion = SchemaVersion
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wkterr.New("WKT_STATE_WRITE", "cannot create the state directory").WithPath(dir)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return wkterr.New("WKT_STATE_WRITE", "cannot encode task state").WithFound(err.Error())
	}
	tmp, err := os.CreateTemp(dir, t.Name+".*.tmp")
	if err != nil {
		return wkterr.New("WKT_STATE_WRITE", "cannot create a temporary state file").WithPath(dir)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_STATE_WRITE", "cannot write task state").WithPath(tmp.Name())
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_STATE_WRITE", "cannot close the temporary state file").WithPath(tmp.Name())
	}
	if err := os.Rename(tmp.Name(), path(dir, t.Name)); err != nil {
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_STATE_WRITE", "cannot commit task state").WithPath(path(dir, t.Name))
	}
	return nil
}

// Load reads a task from disk by name.
func Load(dir, name string) (Task, error) {
	b, err := os.ReadFile(path(dir, name))
	if err != nil {
		return Task{}, wkterr.New("WKT_NO_TASK", "no such task").WithPath(path(dir, name))
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return Task{}, wkterr.New("WKT_STATE_CORRUPT", "task state is not readable").
			WithPath(path(dir, name)).WithFound(err.Error())
	}
	if t.SchemaVersion > SchemaVersion {
		return Task{}, wkterr.New("WKT_STATE_VERSION", "task state was written by a newer wkt").
			WithPath(path(dir, name)).
			WithExpected(strconv.Itoa(SchemaVersion)).
			WithFound(strconv.Itoa(t.SchemaVersion)).
			WithRemedy("upgrade wkt")
	}
	return t, nil
}

// List returns the names of all saved tasks in a directory.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // no tasks yet is not an error
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return out, nil
}

// Container is the container's own state, as opposed to a task's: decisions
// made at init time that every later command has to honour. Spec §5.3 rule 6
// requires the nested-repository exclusions to be "recorded in container
// state", because a workspace whose nested repository was excluded once must
// stay adoptable without repeating the flag.
type Container struct {
	SchemaVersion int      `json:"schema_version"`
	Excluded      []string `json:"excluded,omitempty"`
}

func containerPath(dir string) string { return filepath.Join(dir, "container.json") }

// SaveContainer writes the container state through the same temp-file-then-
// rename dance Save uses, so a reader never sees a partial file.
func SaveContainer(dir string, c Container) error {
	c.SchemaVersion = SchemaVersion
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wkterr.New("WKT_STATE_WRITE", "cannot create the state directory").WithPath(dir)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return wkterr.New("WKT_STATE_WRITE", "cannot encode container state").WithFound(err.Error())
	}
	tmp, err := os.CreateTemp(dir, "container.*.tmp")
	if err != nil {
		return wkterr.New("WKT_STATE_WRITE", "cannot create a temporary state file").WithPath(dir)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_STATE_WRITE", "cannot write container state").WithPath(tmp.Name())
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_STATE_WRITE", "cannot close the temporary state file").WithPath(tmp.Name())
	}
	if err := os.Rename(tmp.Name(), containerPath(dir)); err != nil {
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_STATE_WRITE", "cannot commit container state").WithPath(containerPath(dir))
	}
	return nil
}

// LoadContainer reads the container state. A container that has never
// excluded anything simply has no file, which is the ordinary case and not
// an error — but an unreadable or future-schema file is, because guessing
// there would silently drop an exclusion the user made on purpose.
func LoadContainer(dir string) (Container, error) {
	b, err := os.ReadFile(containerPath(dir))
	if os.IsNotExist(err) {
		return Container{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return Container{}, wkterr.New("WKT_STATE_CORRUPT", "container state is not readable").
			WithPath(containerPath(dir)).WithFound(err.Error())
	}
	var c Container
	if err := json.Unmarshal(b, &c); err != nil {
		return Container{}, wkterr.New("WKT_STATE_CORRUPT", "container state is not readable").
			WithPath(containerPath(dir)).WithFound(err.Error())
	}
	if c.SchemaVersion > SchemaVersion {
		return Container{}, wkterr.New("WKT_STATE_VERSION", "container state was written by a newer wkt").
			WithPath(containerPath(dir)).
			WithExpected(strconv.Itoa(SchemaVersion)).
			WithFound(strconv.Itoa(c.SchemaVersion)).
			WithRemedy("upgrade wkt")
	}
	return c, nil
}
