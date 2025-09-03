package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/notnil/chess"
	"github.com/notnil/chess/uci"
)

// EnginePool manages a pool of UCI chess engines to handle concurrent requests efficiently.
type EnginePool struct {
	engines chan *uci.Engine
	path    string
}

// NewEnginePool creates and initializes a new engine pool.
func NewEnginePool(enginePath string, poolSize int) (*EnginePool, error) {
	pool := &EnginePool{
		// Use a buffered channel to store the available engine instances.
		engines: make(chan *uci.Engine, poolSize),
		path:    enginePath,
	}

	// Pre-start and initialize each engine instance.
	for i := 0; i < poolSize; i++ {
		engine, err := uci.New(enginePath)
		if err != nil {
			// If one fails, close any that were successfully created before failing.
			pool.Close()
			return nil, err
		}

		// Send standard UCI initialization commands.
		if err := engine.Run(uci.CmdUCI, uci.CmdIsReady, uci.CmdUCINewGame); err != nil {
			engine.Close()
			pool.Close()
			return nil, err
		}

		pool.engines <- engine
	}

	log.Printf("Created engine pool with %d Stockfish instances", poolSize)
	return pool, nil
}

// Get retrieves an engine from the pool, blocking until one is available.
func (p *EnginePool) Get() *uci.Engine {
	return <-p.engines
}

// Put returns an engine to the pool so it can be reused.
func (p *EnginePool) Put(engine *uci.Engine) {
	// Use a select with a default case for a non-blocking put.
	// This prevents deadlocks if an engine is put back into a full pool.
	select {
	case p.engines <- engine:
	default:
		// Should not happen with correct Get/Put usage, but as a safeguard,
		// close the engine if the pool is unexpectedly full.
		engine.Close()
	}
}

// Close cleanly shuts down all engines in the pool.
func (p *EnginePool) Close() {
	close(p.engines) // Close the channel to signal no more engines will be added.
	for engine := range p.engines {
		engine.Close()
	}
}

var enginePool *EnginePool

// MoveRequest defines the structure of the JSON request body.
type MoveRequest struct {
	UserMove   string `json:"user_move" binding:"required"`
	CurrentFEN string `json:"current_fen" binding:"required"`
	Depth      int    `json:"depth"`     // Optional: Search depth
	MoveTime   int    `json:"move_time"` // Optional: Thinking time in milliseconds
}

// MoveResponse defines the structure of the JSON response.
type MoveResponse struct {
	UserMoveStatus   string `json:"user_move_status"`
	EngineMove       string `json:"engine_move"`
	EngineMoveStatus string `json:"engine_move_status"`
	NewFEN           string `json:"new_fen"`
	GameOutcome      string `json:"game_outcome"`
}

func main() {
	stockfishPath, err := findStockfish()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Using Stockfish at: %s", stockfishPath)

	// Create engine pool (can be configured via env var, for example)
	poolSize := 5
	enginePool, err = NewEnginePool(stockfishPath, poolSize)
	if err != nil {
		log.Fatalf("Failed to create engine pool: %v", err)
	}

	// gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	router.Use(cors.Default())

	router.POST("/move", moveHandler)

	// --- Graceful Server Shutdown ---
	port := "8080"
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	go func() {
		log.Printf("Starting chess API server with Gin on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Shutting down chess engine pool...")
	enginePool.Close()
	log.Println("Server exiting")
}

// moveHandler is updated to correctly parse UCI moves.
func moveHandler(c *gin.Context) {
	var req MoveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		return
	}

	if req.Depth == 0 && req.MoveTime == 0 {
		req.MoveTime = 1000 // Default to 1 second
	}

	fen, err := chess.FEN(req.CurrentFEN)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid FEN string"})
		return
	}
	game := chess.NewGame(fen)

	// --- 1. Process User's Move (THE FIX IS HERE) ---
	// Instead of game.MoveStr, we manually find the move from the valid moves list.
	// This correctly handles UCI notation like "e2e4".
	userMove, err := findMoveByUCI(game, req.UserMove)
	if err != nil {
		log.Printf("User made an illegal move: %s", req.UserMove)
		c.JSON(http.StatusOK, MoveResponse{
			UserMoveStatus: "illegal-move",
			NewFEN:         req.CurrentFEN,
			GameOutcome:    game.Outcome().String(), // Will be "*" for ongoing game
		})
		return
	}

	// Apply the validated move object.
	if err := game.Move(userMove); err != nil {
		// This should not happen if findMoveByUCI works correctly, but it's good practice to check.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to apply a valid move: " + err.Error()})
		return
	}

	// --- 2. Analyze the game state AFTER the user's move ---
	userMoveStatus := getMoveStatus(game, userMove)
	log.Printf("User move '%s' was legal. Status: %s", req.UserMove, userMoveStatus)

	if game.Outcome() != chess.NoOutcome {
		log.Printf("Game over after user move. Outcome: %s", game.Outcome())
		c.JSON(http.StatusOK, MoveResponse{
			UserMoveStatus: userMoveStatus, // Status could be "check-mate"
			EngineMove:     "",
			NewFEN:         game.Position().String(),
			GameOutcome:    game.Outcome().String(),
		})
		return
	}

	// --- 3. Get Engine's Move (this part is already correct) ---
	engine := enginePool.Get()
	defer enginePool.Put(engine)

	cmdPos := uci.CmdPosition{Position: game.Position()}
	cmdGo := uci.CmdGo{Depth: req.Depth, MoveTime: time.Duration(req.MoveTime) * time.Millisecond}

	if err := engine.Run(cmdPos, cmdGo); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Engine search failed: " + err.Error()})
		return
	}

	results := engine.SearchResults()
	bestMove := results.BestMove
	if bestMove == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Engine could not find a move"})
		return
	}

	if err := game.Move(bestMove); err != nil {
		log.Printf("FATAL: Engine suggested an illegal move: %s", bestMove.String())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: Engine suggested illegal move"})
		return
	}

	// --- 4. Analyze and Respond ---
	engineMoveStatus := getMoveStatus(game, bestMove)
	engineMoveStr := bestMove.String()
	log.Printf("Engine move '%s'. Status: %s", engineMoveStr, engineMoveStatus)

	c.JSON(http.StatusOK, MoveResponse{
		UserMoveStatus:   userMoveStatus,
		EngineMove:       engineMoveStr,
		EngineMoveStatus: engineMoveStatus,
		NewFEN:           game.Position().String(),
		GameOutcome:      game.Outcome().String(),
	})
}

// findMoveByUCI iterates through valid moves to find one matching the given UCI string.
func findMoveByUCI(game *chess.Game, uciMove string) (*chess.Move, error) {
	for _, move := range game.ValidMoves() {
		if move.String() == uciMove {
			return move, nil
		}
	}
	return nil, os.ErrNotExist // Return a clear error if the move isn't found
}

// getMoveStatus analyzes the game state *after* a move has been made.
// It uses the move itself to determine tags like check or capture.
func getMoveStatus(game *chess.Game, move *chess.Move) string {
	// Priority 1: Check for game-ending outcomes first.
	if game.Outcome() != chess.NoOutcome {
		switch game.Method() {
		case chess.Checkmate:
			return "check-mate"
		case chess.Stalemate, chess.FivefoldRepetition, chess.FiftyMoveRule, chess.InsufficientMaterial:
			return "draw"
		default:
			return "draw" // Catch-all for other draw types
		}
	}

	// Priority 2: Check if the move delivered a check.
	if move.HasTag(chess.Check) {
		return "check"
	}

	// Priority 3: Check if the move was a capture.
	if move.HasTag(chess.Capture) {
		return "capture"
	}

	// Otherwise, it was just a legal move.
	return "legal-move"
}

// findStockfish attempts to locate the stockfish executable.
func findStockfish() (string, error) {
	if path := os.Getenv("STOCKFISH_PATH"); path != "" {
		return path, nil
	}
	possiblePaths := []string{
		"stockfish", "/usr/games/stockfish", "/usr/bin/stockfish",
		"/opt/homebrew/bin/stockfish", "/usr/local/bin/stockfish", "./stockfish",
	}
	for _, path := range possiblePaths {
		if p, err := exec.LookPath(path); err == nil {
			return p, nil
		}
	}
	return "", os.ErrNotExist // Return a standard error
}
