package api

import (
	"errors"
	"net/http"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/identity"
)

type entityRequest struct {
	ID       string            `json:"id"` // when set, update this entity in place (keeps its name)
	Name     string            `json:"name"`
	Policies []string          `json:"policies"`
	Metadata map[string]string `json:"metadata"`
	Disabled bool              `json:"disabled"`
}

type groupRequest struct {
	ID              string            `json:"id"` // when set, update this group in place (keeps its name)
	Name            string            `json:"name"`
	Type            string            `json:"type"` // "internal" (default) or "external"
	Policies        []string          `json:"policies"`
	MemberEntityIDs []string          `json:"member_entity_ids"`
	MemberGroupIDs  []string          `json:"member_group_ids"`
	MountType       string            `json:"mount_type"` // external groups: asserting auth method
	GroupName       string            `json:"group_name"` // external groups: asserted group name
	Metadata        map[string]string `json:"metadata"`
}

type entityAliasRequest struct {
	Name        string `json:"name"`
	CanonicalID string `json:"canonical_id"` // the entity ID, matching Vault's field name
	MountType   string `json:"mount_type"`
}

func (h *Handler) identityWriteEntity(w http.ResponseWriter, r *http.Request) {
	var req entityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// An id updates that entity in place (the way to edit an auto-created entity,
	// whose name contains "/"); otherwise create/update by name.
	var (
		ent *identity.Entity
		err error
	)
	if req.ID != "" {
		ent, err = h.identity.UpdateEntity(r.Context(), req.ID, req.Policies, req.Metadata, req.Disabled)
	} else {
		ent, err = h.identity.WriteEntity(r.Context(), req.Name, req.Policies, req.Metadata, req.Disabled)
	}
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, entityData(ent))
}

func (h *Handler) identityReadEntityByID(w http.ResponseWriter, r *http.Request) {
	ent, err := h.identity.ReadEntity(r.Context(), r.PathValue("id"))
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, entityData(ent))
}

func (h *Handler) identityReadEntityByName(w http.ResponseWriter, r *http.Request) {
	ent, err := h.identity.ReadEntityByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, entityData(ent))
}

func (h *Handler) identityListEntities(w http.ResponseWriter, r *http.Request) {
	ids, err := h.identity.ListEntities(r.Context())
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": ids})
}

func (h *Handler) identityDeleteEntity(w http.ResponseWriter, r *http.Request) {
	if err := h.identity.DeleteEntity(r.Context(), r.PathValue("id")); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) identityWriteEntityAlias(w http.ResponseWriter, r *http.Request) {
	var req entityAliasRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	alias, err := h.identity.CreateAlias(r.Context(), req.CanonicalID, req.MountType, req.Name)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, map[string]any{
		"id":           alias.ID,
		"canonical_id": alias.EntityID,
		"mount_type":   alias.MountType,
		"name":         alias.Name,
	})
}

func (h *Handler) identityDeleteEntityAlias(w http.ResponseWriter, r *http.Request) {
	if err := h.identity.DeleteAlias(r.Context(), r.PathValue("id")); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) identityWriteGroup(w http.ResponseWriter, r *http.Request) {
	var req groupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in := identity.GroupInput{
		Type:            req.Type,
		Policies:        req.Policies,
		MemberEntityIDs: req.MemberEntityIDs,
		MemberGroupIDs:  req.MemberGroupIDs,
		MountType:       req.MountType,
		GroupName:       req.GroupName,
		Metadata:        req.Metadata,
	}
	var (
		g   *identity.Group
		err error
	)
	if req.ID != "" {
		g, err = h.identity.UpdateGroup(r.Context(), req.ID, in)
	} else {
		g, err = h.identity.WriteGroup(r.Context(), req.Name, in)
	}
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, groupData(g))
}

func (h *Handler) identityReadGroupByID(w http.ResponseWriter, r *http.Request) {
	g, err := h.identity.ReadGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, groupData(g))
}

func (h *Handler) identityReadGroupByName(w http.ResponseWriter, r *http.Request) {
	g, err := h.identity.ReadGroupByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, groupData(g))
}

func (h *Handler) identityListGroups(w http.ResponseWriter, r *http.Request) {
	ids, err := h.identity.ListGroups(r.Context())
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	writeData(w, map[string]any{"keys": ids})
}

func (h *Handler) identityDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.identity.DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		writeIdentityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func groupData(g *identity.Group) map[string]any {
	return map[string]any{
		"id":                g.ID,
		"name":              g.Name,
		"type":              g.Type,
		"policies":          g.Policies,
		"member_entity_ids": g.MemberEntityIDs,
		"member_group_ids":  g.MemberGroupIDs,
		"mount_type":        g.MountType,
		"group_name":        g.GroupName,
		"metadata":          g.Metadata,
		"created_time":      g.CreatedTime,
	}
}

func entityData(ent *identity.Entity) map[string]any {
	return map[string]any{
		"id":           ent.ID,
		"name":         ent.Name,
		"policies":     ent.Policies,
		"metadata":     ent.Metadata,
		"disabled":     ent.Disabled,
		"created_time": ent.CreatedTime,
	}
}

func writeIdentityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, barrier.ErrSealed):
		writeError(w, http.StatusServiceUnavailable, "vault is sealed")
	case errors.Is(err, identity.ErrEntityNotFound), errors.Is(err, identity.ErrAliasNotFound),
		errors.Is(err, identity.ErrGroupNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, identity.ErrInvalidName), errors.Is(err, identity.ErrNameTaken):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternal(w, err)
	}
}
