package chat

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/auth"
	"github.com/webdesinoprojects/Crikoptions/backend/internal/shared/httpjson"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) ListRooms(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeDomainError(w, domainError(401, "UNAUTHORIZED", "Unauthorized"))
		return
	}
	rooms, err := h.service.ListRooms(r.Context(), userID)
	if err != nil {
		writeInternalError(w)
		return
	}
	httpjson.WriteSuccess(w, http.StatusOK, "Chat rooms fetched", map[string]any{"items": rooms})
}

func (h *Handler) ListMessages(w http.ResponseWriter, r *http.Request) {
	page, domainErr, err := h.service.ListMessages(r.Context(), r.PathValue("roomId"), r.URL.Query().Get("cursor"), parseLimit(r))
	if domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	if err != nil {
		writeInternalError(w)
		return
	}
	httpjson.WriteSuccess(w, http.StatusOK, "Messages fetched", page)
}

type createMessageRequest struct {
	ClientMessageID string `json:"clientMessageId"`
	Text            string `json:"text"`
}

func (h *Handler) CreateMessage(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeDomainError(w, domainError(401, "UNAUTHORIZED", "Unauthorized"))
		return
	}
	var req createMessageRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeDomainError(w, domainError(400, "INVALID_REQUEST", "Invalid request body"))
		return
	}
	message, created, domainErr, err := h.service.CreateMessage(r.Context(), r.PathValue("roomId"), userID, strings.TrimSpace(req.ClientMessageID), req.Text)
	if domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	if err != nil {
		writeInternalError(w)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		log.Printf("chat message created room=%s message=%s author=%s", message.RoomID, message.ID, userID.Hex())
	}
	httpjson.WriteSuccess(w, status, "Message sent", message)
}

type markReadRequest struct {
	LastReadMessageID string `json:"lastReadMessageId"`
}

func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeDomainError(w, domainError(401, "UNAUTHORIZED", "Unauthorized"))
		return
	}
	var req markReadRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeDomainError(w, domainError(400, "INVALID_REQUEST", "Invalid request body"))
		return
	}
	if domainErr := h.service.MarkRead(r.Context(), r.PathValue("roomId"), userID, req.LastReadMessageID); domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	httpjson.WriteSuccess(w, http.StatusOK, "Room marked as read", nil)
}

func (h *Handler) DeleteMessage(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	role, _ := auth.RoleFromContext(r)
	if !ok {
		writeDomainError(w, domainError(401, "UNAUTHORIZED", "Unauthorized"))
		return
	}
	message, changed, domainErr, err := h.service.DeleteMessage(r.Context(), r.PathValue("messageId"), userID, role)
	if domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	if err != nil {
		writeInternalError(w)
		return
	}
	if changed {
		log.Printf("chat message deleted room=%s message=%s actor=%s role=%s", message.RoomID, message.ID, userID.Hex(), role)
	}
	httpjson.WriteSuccess(w, http.StatusOK, "Message deleted", message)
}

type reportMessageRequest struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

func (h *Handler) ReportMessage(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeDomainError(w, domainError(401, "UNAUTHORIZED", "Unauthorized"))
		return
	}
	var req reportMessageRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeDomainError(w, domainError(400, "INVALID_REQUEST", "Invalid request body"))
		return
	}
	report, created, domainErr, err := h.service.ReportMessage(r.Context(), r.PathValue("messageId"), userID, req.Reason, req.Note)
	if domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	if err != nil {
		writeInternalError(w)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		log.Printf("chat report created report=%s message=%s reporter=%s reason=%s", report.ID, report.MessageID, userID.Hex(), report.Reason)
	}
	httpjson.WriteSuccess(w, status, "Report submitted", report)
}

func (h *Handler) ListReports(w http.ResponseWriter, r *http.Request) {
	page, domainErr, err := h.service.ListReports(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("cursor"), parseLimit(r))
	if domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	if err != nil {
		writeInternalError(w)
		return
	}
	httpjson.WriteSuccess(w, http.StatusOK, "Chat reports fetched", page)
}

type resolveReportRequest struct {
	Action string `json:"action"`
}

func (h *Handler) ResolveReport(w http.ResponseWriter, r *http.Request) {
	adminID, ok := auth.UserIDFromContext(r)
	if !ok {
		writeDomainError(w, domainError(401, "UNAUTHORIZED", "Unauthorized"))
		return
	}
	var req resolveReportRequest
	if err := decodeRequest(w, r, &req); err != nil {
		writeDomainError(w, domainError(400, "INVALID_REQUEST", "Invalid request body"))
		return
	}
	report, domainErr, err := h.service.ResolveReport(r.Context(), r.PathValue("reportId"), adminID, req.Action)
	if domainErr != nil {
		writeDomainError(w, domainErr)
		return
	}
	if err != nil {
		writeInternalError(w)
		return
	}
	log.Printf("chat report resolved report=%s message=%s admin=%s action=%s", report.ID, report.MessageID, adminID.Hex(), report.Resolution)
	httpjson.WriteSuccess(w, http.StatusOK, "Report resolved", report)
}

func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

func parseLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return limit
}

func writeDomainError(w http.ResponseWriter, err *DomainError) {
	if err.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(err.RetryAfter.Seconds()))))
	}
	httpjson.WriteError(w, err.Status, err.Code, err.Message, "")
}

func writeInternalError(w http.ResponseWriter) {
	httpjson.WriteError(w, http.StatusInternalServerError, "CHAT_INTERNAL_ERROR", "Unable to complete chat request", "")
}
