package main

import (
	"context"
	"crypto/rand"
	"log"
	"math/big"
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
		engines: make(chan *uci.Engine, poolSize),
		path:    enginePath,
	}

	for i := 0; i < poolSize; i++ {
		engine, err := uci.New(enginePath)
		if err != nil {
			pool.Close()
			return nil, err
		}

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
	select {
	case p.engines <- engine:
	default:
		engine.Close()
	}
}

// Close cleanly shuts down all engines in the pool.
func (p *EnginePool) Close() {
	close(p.engines)
	for engine := range p.engines {
		engine.Close()
	}
}

var enginePool *EnginePool

// MoveRequest defines the structure of the JSON request body.
type MoveRequest struct {
	UserMove   string `json:"user_move" binding:"required"`
	CurrentFEN string `json:"current_fen" binding:"required"`
	Depth      int    `json:"depth"`
	MoveTime   int    `json:"move_time"`
	Elo        int    `json:"elo"`
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

	poolSize := 5
	enginePool, err = NewEnginePool(stockfishPath, poolSize)
	if err != nil {
		log.Fatalf("Failed to create engine pool: %v", err)
	}

	router := gin.Default()

	router.Use(cors.Default())

	router.POST("/move", moveHandler)

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

// moveHandler is updated to use UCI_Elo instead of Skill Level.
func moveHandler(c *gin.Context) {
	var req MoveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		return
	}

	if req.Depth == 0 && req.MoveTime == 0 {
		req.MoveTime = 1000
	}

	fen, err := chess.FEN(req.CurrentFEN)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid FEN string"})
		return
	}
	game := chess.NewGame(fen)

	userMove, err := findMoveByUCI(game, req.UserMove)
	if err != nil {
		log.Printf("User made an illegal move: %s", req.UserMove)
		c.JSON(http.StatusOK, MoveResponse{
			UserMoveStatus: "illegal-move",
			NewFEN:         req.CurrentFEN,
			GameOutcome:    game.Outcome().String(),
		})
		return
	}

	if err := game.Move(userMove); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to apply a valid move: " + err.Error()})
		return
	}

	userMoveStatus := getMoveStatus(game, userMove)
	if game.Outcome() != chess.NoOutcome {
		log.Printf("Game over after user move. Outcome: %s", game.Outcome())
		c.JSON(http.StatusOK, MoveResponse{
			UserMoveStatus: userMoveStatus,
			EngineMove:     "",
			NewFEN:         game.Position().String(),
			GameOutcome:    game.Outcome().String(),
		})
		return
	}

	engine := enginePool.Get()
	defer enginePool.Put(engine)

	const minBlunderElo = 400
	const maxBlunderElo = 2000
	const maxBlunderChance = 40

	var blunderChance int = 0
	if req.Elo >= minBlunderElo && req.Elo < maxBlunderElo {
		progress := float64(req.Elo-minBlunderElo) / float64(maxBlunderElo-minBlunderElo)
		blunderChance = int((1.0 - progress) * float64(maxBlunderChance))
	}

	eloCmds := []uci.Cmd{
		uci.CmdSetOption{Name: "UCI_LimitStrength", Value: "true"},
		uci.CmdSetOption{Name: "UCI_Elo", Value: "1320"},
	}
	if err := engine.Run(eloCmds...); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to set engine ELO: " + err.Error()})
		return
	}

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

	randCheck, _ := rand.Int(rand.Reader, big.NewInt(100))
	if int(randCheck.Int64()) < blunderChance {
		log.Printf("!!! BLUNDERING !!! Chance was %d%%. Finding a random move.", blunderChance)
		allMoves := game.ValidMoves()
		if len(allMoves) > 0 {
			randIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(allMoves))))
			bestMove = allMoves[randIndex.Int64()]
		}
	} else {
		log.Printf("Not blundering. Chance was %d%%.", blunderChance)
	}

	if err := game.Move(bestMove); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error: Engine suggested illegal move"})
		return
	}

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
	return nil, os.ErrNotExist
}

// getMoveStatus analyzes the game state *after* a move has been made.
// It uses the move itself to determine tags like check or capture.
func getMoveStatus(game *chess.Game, move *chess.Move) string {
	if game.Outcome() != chess.NoOutcome {
		switch game.Method() {
		case chess.Checkmate:
			return "check-mate"
		case chess.Stalemate, chess.FivefoldRepetition, chess.FiftyMoveRule, chess.InsufficientMaterial:
			return "draw"
		default:
			return "draw"
		}
	}

	if move.HasTag(chess.Check) {
		return "check"
	}

	if move.HasTag(chess.Capture) {
		return "capture"
	}

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
	return "", os.ErrNotExist
}
