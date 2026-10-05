package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/retyc/retyc-k8s-csi/internal/identity"
)

// StageStore records, on the host, what NodeStageVolume mounted where: kubelet never calls
// NodeStageVolume again for a volume it considers staged, so after a node plugin restart these
// records are the only way to know which dataroom each dead davfs2 mount was serving, and with
// which identity. One JSON file per staging path, written 0600 since a tenant's record holds its
// credentials (the default identity's does not: it comes back from the environment).
type StageStore struct {
	Dir string
}

// stageRecord is one staged volume.
type stageRecord struct {
	StagingPath string `json:"stagingPath"`
	Title       string `json:"title"`
	Namespace   string `json:"namespace,omitempty"`
	Token       string `json:"token,omitempty"`
	Passphrase  string `json:"passphrase,omitempty"`
	// Device is the major:minor of the davfs2 mount the pods' target paths are bound to, which
	// finds them again even once the staging path itself is no longer mounted.
	Device string `json:"device,omitempty"`
}

// credentials returns the record's identity, nil for the default one.
func (r *stageRecord) credentials() *identity.Credentials {
	if r.Token == "" {
		return nil
	}

	return &identity.Credentials{Token: r.Token, Passphrase: r.Passphrase}
}

const stageRecordExt = ".json"

func (s *StageStore) path(stagingPath string) string {
	sum := sha256.Sum256([]byte(stagingPath))

	return filepath.Join(s.Dir, hex.EncodeToString(sum[:16])+stageRecordExt)
}

// Save writes rec atomically, replacing any previous record of the same staging path.
func (s *StageStore) Save(rec *stageRecord) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", s.Dir, err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".stage-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), s.path(rec.StagingPath))
}

// Get returns the record of stagingPath, nil when there is none.
func (s *StageStore) Get(stagingPath string) (*stageRecord, error) {
	data, err := os.ReadFile(s.path(stagingPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // no record is a valid outcome
	}
	if err != nil {
		return nil, err
	}
	rec := &stageRecord{}
	if err := json.Unmarshal(data, rec); err != nil {
		return nil, fmt.Errorf("stage record of %s: %w", stagingPath, err)
	}

	return rec, nil
}

// Remove deletes the record of stagingPath, if any.
func (s *StageStore) Remove(stagingPath string) error {
	if err := os.Remove(s.path(stagingPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// List returns every record. Unreadable ones are skipped and reported in the error, so one
// corrupted file does not prevent recovering the other volumes.
func (s *StageStore) List() ([]*stageRecord, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var (
		records []*stageRecord
		errs    []error
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), stageRecordExt) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			errs = append(errs, err)

			continue
		}
		rec := &stageRecord{}
		if err := json.Unmarshal(data, rec); err != nil || rec.StagingPath == "" || rec.Title == "" {
			errs = append(errs, fmt.Errorf("%s: not a stage record", e.Name()))

			continue
		}
		records = append(records, rec)
	}

	return records, errors.Join(errs...)
}
