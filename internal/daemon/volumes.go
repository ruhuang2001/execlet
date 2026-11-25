package daemon

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	VolumeCapability = "volumes.v1"
	minVolumeBytes   = 16 << 20
	maxVolumeBytes   = 1 << 40
	volumeUnit       = 1 << 20
	maxVolumeMounts  = 8
)

type Volume struct {
	ID        string    `json:"id"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateVolumeRequest struct {
	SizeBytes int64 `json:"size_bytes"`
}

type VolumeMount struct {
	VolumeID string `json:"volume_id"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitzero"`
}

func (m *VolumeMount) UnmarshalJSONFrom(d *jsontext.Decoder) error {
	type plain VolumeMount
	return json.UnmarshalDecode(d, (*plain)(m), json.WithUnmarshalers(json.UnmarshalFunc(func(raw []byte, b *bool) error {
		switch string(raw) {
		case "true", "false":
			*b = string(raw) == "true"
			return nil
		default:
			return errors.New("read_only must be a boolean")
		}
	})))
}

type BindMount struct {
	Source    string
	Target    string
	ReadOnly  bool
	SizeBytes int64
}

func validateVolumeMounts(mounts []VolumeMount) error {
	if len(mounts) > maxVolumeMounts {
		return errorf(CodeInvalidRequest, "at most %d volume mounts are allowed", maxVolumeMounts)
	}
	for i, m := range mounts {
		if !validStateID(m.VolumeID, "vol_") || len(m.Path) > MaxFilePathBytes || path.Clean(m.Path) != m.Path || !strings.HasPrefix(m.Path, "/mnt/") || strings.IndexFunc(m.Path, invalidMountRune) >= 0 {
			return errorf(CodeInvalidRequest, "volume mount needs a valid volume_id and a canonical path below /mnt using letters, digits, slash, dot, underscore or hyphen")
		}
		for _, other := range mounts[:i] {
			if m.VolumeID == other.VolumeID || withinMount(m.Path, other.Path) || withinMount(other.Path, m.Path) {
				return errorf(CodeInvalidRequest, "volume mounts cannot repeat a volume or overlap")
			}
		}
	}
	return nil
}

func invalidMountRune(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("/._-", c):
		return false
	default:
		return true
	}
}

func withinMount(p, target string) bool { return p == target || strings.HasPrefix(p, target+"/") }

type volumeRecord struct {
	Volume
	State string `json:"state"`
	busy  bool
}

type volumeDisk interface {
	create(Volume) error
	ensure(Volume) error
	remove(Volume) error
	path(string) string
}

type volumes struct {
	mu      sync.Mutex
	journal *Journal
	disk    volumeDisk
	limit   int
	maxSize int64
	records map[string]volumeRecord
	holder  map[string]string
}

func (s *Sandboxes) openVolumes(j *Journal, entries []os.DirEntry) error {
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vol_") && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	p, ok := s.backend.(*PolicyBackend)
	if s.volumes == nil && (!ok || p.policy.MaxVolumes == 0) {
		if len(names) != 0 {
			return errors.New("state contains persistent volumes but policy has disabled them")
		}
		return nil
	}
	if s.volumes == nil {
		disk, err := openVolumeDisk(j.dir)
		if err != nil {
			return fmt.Errorf("persistent volumes unavailable: %w", err)
		}
		s.volumes = &volumes{disk: disk, limit: p.policy.MaxVolumes, maxSize: p.policy.MaxVolumeBytes}
	}
	v := s.volumes
	v.journal = j
	v.records = make(map[string]volumeRecord, len(names))
	v.holder = make(map[string]string)
	for _, name := range names {
		var entry volumeRecord
		if err := j.read(name, &entry); err != nil {
			return fmt.Errorf("read volume record: %w", err)
		}
		if !validStateID(entry.ID, "vol_") || name != entry.ID+".json" || !validVolumeSize(entry.SizeBytes) || entry.CreatedAt.IsZero() {
			return errors.New("invalid volume record")
		}
		switch entry.State {
		case "creating", "deleting":
		case "ready":
			if err := v.disk.ensure(entry.Volume); err != nil {
				return fmt.Errorf("restore volume %s: %w", entry.ID, err)
			}
		default:
			return errors.New("invalid volume state")
		}
		v.records[entry.ID] = entry
	}
	return nil
}

func validVolumeSize(n int64) bool {
	return n >= minVolumeBytes && n <= maxVolumeBytes && n%volumeUnit == 0
}

func (s *Sandboxes) requireVolumes() (*volumes, error) {
	if s.volumes == nil {
		return nil, errorf(CodeUnavailable, "persistent volumes are not enabled")
	}
	return s.volumes, s.durableReady()
}

func (s *Sandboxes) CreateVolume(req CreateVolumeRequest) (Volume, error) {
	v, err := s.requireVolumes()
	if err != nil {
		return Volume{}, err
	}
	if !validVolumeSize(req.SizeBytes) || req.SizeBytes > v.maxSize {
		return Volume{}, errorf(CodeInvalidRequest, "size_bytes must be a whole MiB between %d and %d", minVolumeBytes, v.maxSize)
	}
	v.mu.Lock()
	if len(v.records) >= v.limit {
		v.mu.Unlock()
		return Volume{}, errorf(CodeRateLimited, "deployment volume count limit reached")
	}
	entry := volumeRecord{ID: newID("vol_"), SizeBytes: req.SizeBytes, CreatedAt: s.now(), State: "creating", busy: true}
	v.records[entry.ID] = entry
	v.mu.Unlock()
	if err := v.persist(entry); err != nil {
		return Volume{}, err
	}
	if err := v.disk.create(entry.Volume); err != nil {
		cleanup := v.remove(entry)
		return Volume{}, &Error{Code: CodeUnavailable, Message: "cannot provision bounded volume storage", Err: errors.Join(err, cleanup)}
	}
	entry.State = "ready"
	if err := v.persist(entry); err != nil {
		return Volume{}, err
	}
	entry.busy = false
	v.mu.Lock()
	v.records[entry.ID] = entry
	v.mu.Unlock()
	return entry.Volume, nil
}

func (s *Sandboxes) ListVolumes() ([]Volume, error) {
	v, err := s.requireVolumes()
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	list := make([]Volume, 0, len(v.records))
	for _, entry := range v.records {
		if entry.State == "ready" {
			list = append(list, entry.Volume)
		}
	}
	slices.SortFunc(list, func(a, b Volume) int { return cmp.Compare(a.ID, b.ID) })
	return list, nil
}

func (s *Sandboxes) GetVolume(id string) (Volume, error) {
	v, err := s.requireVolumes()
	if err != nil {
		return Volume{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entry := v.records[id]
	if entry.State != "ready" {
		return Volume{}, errorf(CodeNotFound, "volume %s does not exist", id)
	}
	return entry.Volume, nil
}

func (s *Sandboxes) DeleteVolume(id string) error {
	v, err := s.requireVolumes()
	if err != nil {
		return err
	}
	v.mu.Lock()
	entry, ok := v.records[id]
	if !ok || entry.State == "creating" && entry.busy {
		v.mu.Unlock()
		return errorf(CodeNotFound, "volume %s does not exist", id)
	}
	if v.holder[id] != "" {
		v.mu.Unlock()
		return errorf(CodeVolumeInUse, "volume %s has a sandbox attachment", id)
	}
	if entry.busy {
		v.mu.Unlock()
		return errorf(CodeUnavailable, "volume storage removal in progress")
	}
	entry.State = "deleting"
	entry.busy = true
	v.records[id] = entry
	v.mu.Unlock()
	if err := v.persist(entry); err != nil {
		return err
	}
	return v.remove(entry)
}

func (v *volumes) persist(entry volumeRecord) error {
	if err := v.journal.write(entry.ID+".json", entry); err != nil {
		return errorf(CodeUnavailable, "cannot persist volume state")
	}
	return nil
}

func (v *volumes) remove(entry volumeRecord) error {
	if err := v.disk.remove(entry.Volume); err != nil {
		entry.busy = false
		v.mu.Lock()
		v.records[entry.ID] = entry
		v.mu.Unlock()
		return &Error{Code: CodeUnavailable, Message: "volume storage removal unconfirmed", Err: err}
	}
	v.journal.remove(entry.ID + ".json")
	if err := v.journal.health(); err != nil {
		return errorf(CodeUnavailable, "cannot persist volume removal")
	}
	v.mu.Lock()
	delete(v.records, entry.ID)
	v.mu.Unlock()
	return nil
}

func (v *volumes) bind(id string, mounts []VolumeMount) ([]BindMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	if v == nil {
		return nil, errorf(CodeUnavailable, "persistent volumes are not enabled")
	}
	v.mu.Lock()
	entries := make([]Volume, 0, len(mounts))
	for _, mount := range mounts {
		entry := v.records[mount.VolumeID]
		if entry.State != "ready" {
			v.mu.Unlock()
			return nil, errorf(CodeNotFound, "volume %s does not exist", mount.VolumeID)
		}
		if v.holder[mount.VolumeID] != "" {
			v.mu.Unlock()
			return nil, errorf(CodeVolumeInUse, "volume %s has a sandbox attachment", mount.VolumeID)
		}
		entries = append(entries, entry.Volume)
	}
	for _, mount := range mounts {
		v.holder[mount.VolumeID] = id
	}
	v.mu.Unlock()
	binds := make([]BindMount, 0, len(mounts))
	for i, mount := range mounts {
		if err := v.disk.ensure(entries[i]); err != nil {
			v.release(id)
			return nil, &Error{Code: CodeUnavailable, Message: "cannot verify bounded volume storage", Err: err}
		}
		binds = append(binds, BindMount{Source: v.disk.path(mount.VolumeID), Target: mount.Path, ReadOnly: mount.ReadOnly, SizeBytes: entries[i].SizeBytes})
	}
	return binds, nil
}

func (s *Sandboxes) resolveVolumeCreate(ctx context.Context, sb *sandbox) error {
	policy, ok := s.backend.(*PolicyBackend)
	if !ok {
		return errors.New("volume recovery requires a deployment policy")
	}
	resolver, ok := policy.Backend.(interface {
		ResolveVolumeCreate(context.Context, string, string) (string, error)
	})
	if !ok {
		return errors.New("backend cannot resolve pending volume attachments")
	}
	call, cancel := backendCtx(ctx)
	id, err := resolver.ResolveVolumeCreate(call, sb.id, s.owner)
	cancel()
	if err != nil {
		return err
	}
	previous := sb.container
	sb.container = id
	if err := s.saveSandboxLocked(sb); err != nil {
		sb.container = previous
		return err
	}
	policy.rekeyAllocation(previous, id)
	return nil
}

func (v *volumes) cleanIncomplete(startup bool) error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	var pending []volumeRecord
	for _, entry := range v.records {
		if (entry.State == "creating" || startup && entry.State == "deleting") && !entry.busy && v.holder[entry.ID] == "" {
			entry.busy = true
			v.records[entry.ID] = entry
			pending = append(pending, entry)
		}
	}
	v.mu.Unlock()
	var failures []error
	for _, entry := range pending {
		if err := v.remove(entry); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Sandboxes) retryVolumeRollbacks(ctx context.Context) {
	tick := time.NewTicker(s.cleanupInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.durableReady() == nil && s.volumes.cleanIncomplete(false) != nil {
				s.metrics.cleanupFailed()
			}
		}
	}
}

func (v *volumes) release(id string) {
	if v == nil || id == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for volumeID, holder := range v.holder {
		if holder == id {
			delete(v.holder, volumeID)
		}
	}
}
