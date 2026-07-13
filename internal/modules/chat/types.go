package chat

import (
	"fmt"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	GlobalRoomID       = "global"
	MaxMessageRunes    = 1000
	MaxReportNoteRunes = 250
)

type DomainError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *DomainError) Error() string { return e.Message }

func domainError(status int, code, message string) *DomainError {
	return &DomainError{Status: status, Code: code, Message: message}
}

var (
	errRoomNotFound      = domainError(http.StatusNotFound, "ROOM_NOT_FOUND", "Chat room not found")
	errRoomReadOnly      = domainError(http.StatusConflict, "ROOM_READ_ONLY", "This match room is archived and read-only")
	errMessageNotFound   = domainError(http.StatusNotFound, "MESSAGE_NOT_FOUND", "Message not found")
	errMessageForbidden  = domainError(http.StatusForbidden, "MESSAGE_FORBIDDEN", "You cannot perform this action on this message")
	errInvalidMessage    = domainError(http.StatusBadRequest, "INVALID_MESSAGE", fmt.Sprintf("Message must be between 1 and %d characters", MaxMessageRunes))
	errInvalidClientID   = domainError(http.StatusBadRequest, "INVALID_CLIENT_MESSAGE_ID", "clientMessageId must be a UUID")
	errInvalidReport     = domainError(http.StatusBadRequest, "INVALID_REPORT", "Invalid report reason or note")
	errReportForbidden   = domainError(http.StatusForbidden, "REPORT_FORBIDDEN", "You cannot report this message")
	errReportExists      = domainError(http.StatusConflict, "REPORT_ALREADY_EXISTS", "You have already reported this message")
	errReportNotFound    = domainError(http.StatusNotFound, "REPORT_NOT_FOUND", "Report not found")
	errInvalidResolution = domainError(http.StatusBadRequest, "INVALID_RESOLUTION", "Resolution must be dismiss or delete_message")
)

type AuthorSnapshot struct {
	ID   primitive.ObjectID `bson:"id" json:"-"`
	Name string             `bson:"name" json:"name"`
	Tier string             `bson:"tier" json:"tier"`
	Role string             `bson:"role" json:"role"`
}

type Message struct {
	ID              primitive.ObjectID  `bson:"_id,omitempty"`
	RoomID          string              `bson:"roomId"`
	Author          AuthorSnapshot      `bson:"author"`
	Text            string              `bson:"text"`
	ClientMessageID string              `bson:"clientMessageId"`
	CreatedAt       time.Time           `bson:"createdAt"`
	DeletedAt       *time.Time          `bson:"deletedAt,omitempty"`
	DeletedBy       *primitive.ObjectID `bson:"deletedBy,omitempty"`
	DeletedByRole   string              `bson:"deletedByRole,omitempty"`
}

type MessageResponse struct {
	ID              string         `json:"id"`
	RoomID          string         `json:"roomId"`
	Author          AuthorResponse `json:"author"`
	Text            string         `json:"text"`
	ClientMessageID string         `json:"clientMessageId"`
	CreatedAt       time.Time      `json:"createdAt"`
	Deleted         bool           `json:"deleted"`
	DeletedAt       *time.Time     `json:"deletedAt,omitempty"`
}

type AuthorResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tier string `json:"tier"`
	Role string `json:"role"`
}

func messageResponse(message Message) MessageResponse {
	text := message.Text
	if message.DeletedAt != nil {
		text = ""
	}
	return MessageResponse{
		ID:     message.ID.Hex(),
		RoomID: message.RoomID,
		Author: AuthorResponse{
			ID: message.Author.ID.Hex(), Name: message.Author.Name,
			Tier: message.Author.Tier, Role: message.Author.Role,
		},
		Text: text, ClientMessageID: message.ClientMessageID,
		CreatedAt: message.CreatedAt, Deleted: message.DeletedAt != nil,
		DeletedAt: message.DeletedAt,
	}
}

type RoomResponse struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Title           string     `json:"title"`
	MatchStatus     string     `json:"matchStatus,omitempty"`
	Writable        bool       `json:"writable"`
	UnreadCount     int64      `json:"unreadCount"`
	LatestMessageAt *time.Time `json:"latestMessageAt,omitempty"`
}

type Report struct {
	ID            primitive.ObjectID  `bson:"_id,omitempty"`
	MessageID     primitive.ObjectID  `bson:"messageId"`
	RoomID        string              `bson:"roomId"`
	MessageText   string              `bson:"messageText"`
	MessageAuthor AuthorSnapshot      `bson:"messageAuthor"`
	ReporterID    primitive.ObjectID  `bson:"reporterId"`
	Reason        string              `bson:"reason"`
	Note          string              `bson:"note,omitempty"`
	Status        string              `bson:"status"`
	CreatedAt     time.Time           `bson:"createdAt"`
	ResolvedAt    *time.Time          `bson:"resolvedAt,omitempty"`
	ResolvedBy    *primitive.ObjectID `bson:"resolvedBy,omitempty"`
	Resolution    string              `bson:"resolution,omitempty"`
}

type ReportResponse struct {
	ID            string         `json:"id"`
	MessageID     string         `json:"messageId"`
	RoomID        string         `json:"roomId"`
	MessageText   string         `json:"messageText"`
	MessageAuthor AuthorResponse `json:"messageAuthor"`
	ReporterID    string         `json:"reporterId"`
	Reason        string         `json:"reason"`
	Note          string         `json:"note,omitempty"`
	Status        string         `json:"status"`
	CreatedAt     time.Time      `json:"createdAt"`
	ResolvedAt    *time.Time     `json:"resolvedAt,omitempty"`
	Resolution    string         `json:"resolution,omitempty"`
}

func reportResponse(report Report) ReportResponse {
	return ReportResponse{
		ID: report.ID.Hex(), MessageID: report.MessageID.Hex(), RoomID: report.RoomID,
		MessageText:   report.MessageText,
		MessageAuthor: AuthorResponse{ID: report.MessageAuthor.ID.Hex(), Name: report.MessageAuthor.Name, Tier: report.MessageAuthor.Tier, Role: report.MessageAuthor.Role},
		ReporterID:    report.ReporterID.Hex(), Reason: report.Reason, Note: report.Note,
		Status: report.Status, CreatedAt: report.CreatedAt, ResolvedAt: report.ResolvedAt, Resolution: report.Resolution,
	}
}

type ReadState struct {
	UserID     primitive.ObjectID `bson:"userId"`
	RoomID     string             `bson:"roomId"`
	LastReadAt time.Time          `bson:"lastReadAt"`
}

type MessagePage struct {
	Items      []MessageResponse `json:"items"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

type ReportPage struct {
	Items      []ReportResponse `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type ChatEvent struct {
	Type    string          `json:"type"`
	Message MessageResponse `json:"message"`
}
