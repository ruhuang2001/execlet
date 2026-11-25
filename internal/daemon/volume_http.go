package daemon

import "net/http"

func (h *Handler) createVolume(w http.ResponseWriter, r *http.Request) {
	var req CreateVolumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	v, err := h.store.CreateVolume(req)
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	w.Header().Set("Location", "/v1/volumes/"+v.ID)
	writeJSON(w, http.StatusCreated, v)
}

func (h *Handler) listVolumes(w http.ResponseWriter, r *http.Request) {
	volumes, err := h.store.ListVolumes()
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": volumes})
}

func (h *Handler) getVolume(w http.ResponseWriter, r *http.Request) {
	v, err := h.store.GetVolume(r.PathValue("volume_id"))
	if err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) deleteVolume(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteVolume(r.PathValue("volume_id")); err != nil {
		writeDaemonError(r.Context(), w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
