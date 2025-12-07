package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type MirrorState int

const (
	MirrorFresh MirrorState = iota + 1
	MirrorReady
)

func (ms MirrorState) String() string {
	switch ms {
	case MirrorFresh:
		return "Fresh"
	case MirrorReady:
		return "Ready"
	default:
		return "Unknown"
	}
}

func ParseMirrorState(str string) (MirrorState, error) {
	switch str {
	case "Fresh":
		return MirrorFresh, nil
	case "Ready":
		return MirrorReady, nil
	}
	return 0, fmt.Errorf("invalid MirrorState: %s", str)
}

func (ms MirrorState) MarshalText() ([]byte, error) {
	return []byte(ms.String()), nil
}

func (ms *MirrorState) UnmarshalText(bs []byte) error {
	val, err := ParseMirrorState(string(bs))
	if err != nil {
		return err
	}

	*ms = val
	return nil
}

type Mirror struct {
	Url        string      `json:"url"`
	LastUpdate time.Time   `json:"last_update"`
	State      MirrorState `json:"state"`
}

type State struct {
	StoragePath string            `json:"storage_path"`
	UpdateDelta time.Duration     `json:"update_delta"`
	Mirrors     map[string]Mirror `json:"mirrors"`
}

func NewState() State {
	return State{UpdateDelta: 5 * time.Minute, Mirrors: make(map[string]Mirror)}
}

type Message struct {
	mirrorName string
}

type Mirrorer struct {
	state    State
	dbPath   string
	messages chan string
}

func (m Mirrorer) updateDb() error {
	log.Println("updating db")

	tempFile, err := os.CreateTemp(filepath.Dir(m.dbPath), "looter.*.tmp")
	if err != nil {
		return fmt.Errorf("cannot create temporary state file: %s", err)
	}

	encoder := json.NewEncoder(tempFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(m.state); err != nil {
		return fmt.Errorf("cannot write temporary state file: %s", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %s", err)
	}

	log.Println("moving", tempFile.Name(), "to", m.dbPath)
	if err := os.Rename(tempFile.Name(), m.dbPath); err != nil {
		return fmt.Errorf("failed to move temp file to state file: %s", err)
	}

	return nil
}

func NewMirrorer(dbPath string) (*Mirrorer, error) {
	if !fs.ValidPath(dbPath) {
		return nil, fmt.Errorf("invalid db path: '%s'", dbPath)
	}

	dbFile, err := os.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open db file: %s", err)
	}

	decoder := json.NewDecoder(dbFile)
	state := NewState()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("failed to decode db: %s", err)
	}
	log.Printf("read state from '%s'\n", dbPath)

	mirrorer := &Mirrorer{
		dbPath: dbPath, state: state, messages: make(chan string)}
	mirrorer.updateDb()
	go mirrorer.Run()

	return mirrorer, nil
}

func checkArgs(w http.ResponseWriter, values url.Values, args ...string) bool {
	missingArgs := make([]string, 0)
	for _, arg := range args {
		if !values.Has(arg) {
			missingArgs = append(missingArgs, arg)
		}
	}

	if len(missingArgs) > 0 {
		http.Error(w, fmt.Sprintf("missing arguments: %s", strings.Join(missingArgs, ", ")), 500)
		return false
	}

	return true
}

func (m *Mirrorer) AddMirrorHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !checkArgs(w, q, "name", "url") {
		return
	}

	mirrorName := q.Get("name")
	mirrorUrl := q.Get("url")

	if _, exists := m.state.Mirrors[mirrorName]; exists {
		http.Error(
			w,
			fmt.Sprintf("cannot add mirror '%s': already exists", mirrorName),
			500)
		return
	}
	m.state.Mirrors[mirrorName] = Mirror{
		Url:        mirrorUrl,
		LastUpdate: time.Now(),
		State:      MirrorFresh,
	}

	if err := m.updateDb(); err != nil {
		http.Error(w, fmt.Sprintf("failed to update db: %s", err), 500)
		return
	}
}

func (m *Mirrorer) ListMirrorsHandler(w http.ResponseWriter, r *http.Request) {
	if err := json.NewEncoder(w).Encode(m.state.Mirrors); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (m *Mirrorer) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/add-mirror", m.AddMirrorHandler)
	mux.HandleFunc("/api/list-mirrors", m.ListMirrorsHandler)
	return mux
}

func (m *Mirrorer) Run() {
	for mirrorName, mirror := range m.state.Mirrors {
		if mirror.State == MirrorFresh {
			if err := gitCloneMirror(m.state.StoragePath, mirrorName, mirror.Url); err != nil {
				log.Println(err)
			}
			mirror.State = MirrorReady
			mirror.LastUpdate = time.Now()
			m.state.Mirrors[mirrorName] = mirror
			if err := m.updateDb(); err != nil {
				log.Println(err)
			}
		}
	}

	for mirrorName, mirror := range m.state.Mirrors {
		if mirror.State == MirrorReady {
			nextUpdateTime := mirror.LastUpdate.Add(m.state.UpdateDelta)
			fmt.Printf("will update '%s' at '%s'\n", mirrorName, nextUpdateTime)
			go func() {
				time.Sleep(time.Until(nextUpdateTime))
				m.messages <- mirrorName
			}()
		}
	}

	for {
		mirrorName := <-m.messages
		mirror := m.state.Mirrors[mirrorName]
		if mirror.State == MirrorReady {
			fmt.Println("updating", mirrorName)
			if err := gitRemoteUpdate(m.state.StoragePath, mirrorName); err != nil {
				log.Println("failed to update mirror:", err)
			} else {
				fmt.Println("successfully updated", mirrorName)
				mirror.LastUpdate = time.Now()
				m.state.Mirrors[mirrorName] = mirror
				m.updateDb()
				nextUpdateTime := mirror.LastUpdate.Add(m.state.UpdateDelta)
				fmt.Printf("will update '%s' at '%s'\n", mirrorName, nextUpdateTime)
				go func() {
					time.Sleep(time.Until(nextUpdateTime))
					m.messages <- mirrorName
				}()
			}
		}
	}
}

func gitCloneMirror(storagePath string, mirrorName string, mirrorUrl string) error {
	dir := filepath.Join(storagePath, mirrorName)

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to clean target dir: %s", err)
	}

	log.Printf("mirroring '%s' -> '%s'\n", mirrorName, dir)
	cmd := exec.Command("git", "clone", "--mirror", mirrorUrl, dir)
	_, err := cmd.Output()
	if err != nil {
		exitError := err.(*exec.ExitError)
		if exitError != nil {
			return fmt.Errorf("mirror failed: %s\n%s\n", err, exitError.Stderr)
		}
		return fmt.Errorf("mirror failed: %s", err)
	}
	log.Printf("successfully mirrored '%s' -> '%s'\n", mirrorName, dir)

	return nil
}

func gitRemoteUpdate(storagePath string, mirrorName string) error {
	dir := filepath.Join(storagePath, mirrorName)

	cmd := exec.Command("git", "-C", dir, "remote", "update")
	_, err := cmd.Output()
	if err != nil {
		exitError := err.(*exec.ExitError)
		if exitError != nil {
			return fmt.Errorf("update failed: %s\n%s\n", err, exitError.Stderr)
		}
		return fmt.Errorf("update failed: %s", err)
	}

	return nil
}

func main() {
	m, err := NewMirrorer("db.json")
	if err != nil {
		log.Fatal(err)
	}

	log.Fatal(http.ListenAndServe(":8080", m.Routes()))
}
