package daemon

import (
	"context"
	"net/http"
	"path"
)

func agentInfo(b Backend, id string) (string, bool) {
	if v, ok := b.(interface{ AgentInfo(string) (string, bool) }); ok {
		return v.AgentInfo(id)
	}
	return "legacy", false
}
func (b *PolicyBackend) AgentInfo(id string) (string, bool) { return agentInfo(b.Backend, id) }

type fileStatter interface {
	StatFile(context.Context, string, string) (FileEntry, error)
}

func statFile(ctx context.Context, b Backend, id, p string) (FileEntry, error) {
	if st, ok := b.(fileStatter); ok {
		return st.StatFile(ctx, id, p)
	}
	parent, name := path.Split(p)
	for after := ""; ; {
		entries, err := b.ListFiles(ctx, id, path.Clean(parent), after, 1000)
		if err != nil {
			return FileEntry{}, err
		}
		for _, e := range entries {
			if e.Name == name {
				return e, nil
			}
		}
		if len(entries) <= 1000 {
			return FileEntry{}, ErrNotFound
		}
		after = entries[999].Name
	}
}
func (b *PolicyBackend) StatFile(ctx context.Context, id, p string) (FileEntry, error) {
	return statFile(ctx, b.Backend, id, p)
}
func (h *Handler) statFile(w http.ResponseWriter, r *http.Request) {
	q, err := strictFileQuery(r, "path")
	if err == nil {
		err = ValidateFilePath(q["path"])
	}
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), FileOperationTimeout)
	defer cancel()
	sb, err := h.store.prepareFiles(ctx, r.PathValue("sandbox_id"))
	if err != nil {
		writeDaemonError(ctx, w, err)
		return
	}
	sb.mu.Lock()
	id := sb.container
	sb.mu.Unlock()
	entry, err := statFile(ctx, h.store.backend, id, path.Clean(q["path"]))
	if err != nil {
		writeDaemonError(ctx, w, backendError(err))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}
