package notification

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Heartbeat intervals (ADR-6): server pings every 30s; a client must pong
// within 60s or the connection is reaped.
const (
	pingInterval  = 30 * time.Second
	pongWait      = 60 * time.Second
	writeWait     = 10 * time.Second
	maxMessageLen = 512 // clients only send control frames (ping/pong)
)

// Gateway manages WebSocket connections keyed by user ID (ADR-6). At current
// scale a per-instance in-memory registry is sufficient; horizontal scale-out
// would add a Redis-backed fan-out (documented future step).
type Gateway struct {
	upgrader websocket.Upgrader
	mu       sync.RWMutex
	conns    map[uuid.UUID]map[*websocket.Conn]struct{}
	metrics  MetricsRecorder
	logger   *slog.Logger
}

// MetricsRecorder records the active-connection gauge (platform metrics).
type MetricsRecorder interface {
	SetWSConnections(n float64)
}

// NewGateway builds a Gateway. allowedOrigins controls the upgrade check;
// nil/empty allows same-origin only.
func NewGateway(allowedOrigins []string, metrics MetricsRecorder, logger *slog.Logger) *Gateway {
	allowed := map[string]struct{}{}
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return &Gateway{
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // non-browser client
				}
				_, ok := allowed[origin]
				return ok
			},
		},
		conns:   map[uuid.UUID]map[*websocket.Conn]struct{}{},
		metrics: metrics,
		logger:  logger,
	}
}

// HandleUpgrade authenticates via the Bearer JWT (passed as a query param
// `token` because browsers can't set headers on WebSocket handshakes — the
// ADR-6 replacement for v1's SSE stream-token workaround), then registers
// the connection.
func (g *Gateway) HandleUpgrade(tokens TokenVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := g.resolveUser(w, r, tokens)
		if !ok {
			return
		}
		conn, err := g.upgrader.Upgrade(w, r, nil)
		if err != nil {
			g.logger.Warn("ws upgrade failed", slog.String("error", err.Error()))
			return
		}
		g.register(userID, conn)
		defer g.unregister(userID, conn)
		g.serveConnection(userID, conn)
	}
}

// resolveUser authenticates the WS handshake: Bearer JWT from middleware
// first, then the `token` query param (browsers can't set headers on the
// WebSocket handshake — ADR-6 replacement for v1's SSE stream token).
func (g *Gateway) resolveUser(w http.ResponseWriter, r *http.Request, tokens TokenVerifier) (uuid.UUID, bool) {
	if id, ok := httpx.UserID(r.Context()); ok {
		return id, true
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "missing token"))
		return uuid.Nil, false
	}
	claims, err := tokens.Verify(token)
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid token"))
		return uuid.Nil, false
	}
	id, err := uuid.Parse(claims.UserID)
	if err != nil {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid subject"))
		return uuid.Nil, false
	}
	return id, true
}

// serveConnection runs the read loop that detects disconnects and serves
// pongs until the client goes away.
func (g *Gateway) serveConnection(userID uuid.UUID, conn *websocket.Conn) {
	conn.SetReadLimit(maxMessageLen)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	// Client-initiated JSON pings are answered with a JSON pong; the
	// frontend composable sends {"type":"ping"} on a 30s interval.
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				g.logger.Info("ws closed unexpectedly", slog.String("userId", userID.String()), slog.String("error", err.Error()))
			}
			return
		}
	}
}

// PushToUser sends a notification to every open connection for the user.
// Safe to call from any goroutine (consumers).
func (g *Gateway) PushToUser(userID uuid.UUID, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		g.logger.Error("ws marshal payload", slog.String("error", err.Error()))
		return
	}
	g.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(g.conns[userID]))
	for c := range g.conns[userID] {
		conns = append(conns, c)
	}
	g.mu.RUnlock()
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(writeWait))
		if err := c.WriteMessage(websocket.TextMessage, raw); err != nil {
			g.logger.Warn("ws write failed, dropping connection", slog.String("userId", userID.String()), slog.String("error", err.Error()))
			g.unregister(userID, c)
			_ = c.Close()
		}
	}
}

// ConnectedUsers returns the number of users with ≥1 live connection.
func (g *Gateway) ConnectedUsers() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.conns)
}

func (g *Gateway) register(userID uuid.UUID, conn *websocket.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conns[userID] == nil {
		g.conns[userID] = map[*websocket.Conn]struct{}{}
	}
	g.conns[userID][conn] = struct{}{}
	if g.metrics != nil {
		g.metrics.SetWSConnections(float64(g.ConnectedUsers()))
	}
}

func (g *Gateway) unregister(userID uuid.UUID, conn *websocket.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if set, ok := g.conns[userID]; ok {
		delete(set, conn)
		if len(set) == 0 {
			delete(g.conns, userID)
		}
	}
	if g.metrics != nil {
		g.metrics.SetWSConnections(float64(g.ConnectedUsers()))
	}
}

// TokenVerifier is the narrow identity dependency for WS auth (composition
// root binds identity.TokenService).
type TokenVerifier interface {
	Verify(token string) (claims TokenClaims, err error)
}

// TokenClaims mirrors identity.Claims without importing the module (ADR-1).
type TokenClaims struct {
	UserID string
}
