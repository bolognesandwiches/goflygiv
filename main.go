package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

type ScanData struct {
	UserID    string          `json:"user_id"`
	ScanType  string          `json:"scan_type"` // "inventory" or "room"
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"` // Store the full scan data as JSON
}

type TradeData struct {
	UID       string      `json:"uid"`
	Date      time.Time   `json:"date"`
	Trader    string      `json:"trader"`
	Recipient string      `json:"recipient"`
	Items     []TradeItem `json:"items"`
}

type TradeItem struct {
	Name    string  `json:"name"`
	ItemID  int     `json:"item_id"`
	HCValue float64 `json:"hc_value"`
}

var db *sql.DB

func main() {
	// Get the DATABASE_URL from environment variables
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is not set")
	}

	// Parse the URL and add sslmode=disable if it's not already there
	parsedURL, err := url.Parse(dbURL)
	if err != nil {
		log.Fatalf("Error parsing DATABASE_URL: %v", err)
	}
	query := parsedURL.Query()
	query.Set("sslmode", "disable")
	parsedURL.RawQuery = query.Encode()

	var dbErr error
	db, dbErr = sql.Open("postgres", parsedURL.String())
	if dbErr != nil {
		log.Fatalf("Error opening database: %v", dbErr)
	}
	defer db.Close()

	// Test the database connection
	if err := db.Ping(); err != nil {
		log.Fatalf("Error connecting to the database: %v", err)
	}

	log.Println("Successfully connected to the database")

	// Initialize the database
	if err := initDB(); err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	r := gin.Default()
	r.POST("/scan", authenticateAPIKey(recordScan))
	r.GET("/scans/:user_id", getUserScans)
	r.POST("/trade", authenticateAPIKey(recordTrade))
	r.GET("/trade", authenticateAPIKey(getAllTradeUIDs))
	r.GET("/trade/:uid", authenticateAPIKey(getTradeByUID))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Starting server on port %s", port)
	if err := r.Run("0.0.0.0:" + port); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}

func initDB() error {
	// Create trades table if it doesn't exist
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS trades (
			uid UUID PRIMARY KEY,
			date TIMESTAMP,
			trader TEXT,
			recipient TEXT,
			items JSONB
		)
	`)
	return err
}

func authenticateAPIKey(f gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("Authorization")
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
			c.Abort()
			return
		}

		// Check the API key against the environment variable
		expectedAPIKey := os.Getenv("API_KEY")
		if apiKey != "Bearer "+expectedAPIKey {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		f(c)
	}
}

func recordScan(c *gin.Context) {
	var scanData ScanData
	if err := c.BindJSON(&scanData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, err := db.Exec(`
        INSERT INTO scans (user_id, scan_type, timestamp, data)
        VALUES ($1, $2, $3, $4)
    `, scanData.UserID, scanData.ScanType, scanData.Timestamp, scanData.Data)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "recorded"})
}

func getUserScans(c *gin.Context) {
	userID := c.Param("user_id")

	// Use ILIKE for case-insensitive matching
	rows, err := db.Query("SELECT scan_type, timestamp, data FROM scans WHERE LOWER(user_id) = LOWER($1)", userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var scans []ScanData
	for rows.Next() {
		var scan ScanData
		var timestamp string
		if err := rows.Scan(&scan.ScanType, &timestamp, &scan.Data); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		scan.UserID = userID // Use the original userID to maintain the case
		scan.Timestamp = timestamp
		scans = append(scans, scan)
	}

	c.JSON(http.StatusOK, scans)
}

func recordTrade(c *gin.Context) {
	var tradeData TradeData
	if err := c.BindJSON(&tradeData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tradeData.UID = uuid.New().String()
	itemsJSON, err := json.Marshal(tradeData.Items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to marshal items data"})
		return
	}

	_, err = db.Exec(`
		INSERT INTO trades (uid, date, trader, recipient, items)
		VALUES ($1, $2, $3, $4, $5)
	`, tradeData.UID, tradeData.Date, tradeData.Trader, tradeData.Recipient, itemsJSON)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "recorded", "uid": tradeData.UID})
}

func getAllTradeUIDs(c *gin.Context) {
	// Parse query parameters for date filtering
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")

	query := "SELECT uid, date FROM trades"
	var args []interface{}
	if startDate != "" && endDate != "" {
		query += " WHERE date BETWEEN $1 AND $2"
		args = append(args, startDate, endDate)
	} else if startDate != "" {
		query += " WHERE date >= $1"
		args = append(args, startDate)
	} else if endDate != "" {
		query += " WHERE date <= $1"
		args = append(args, endDate)
	}

	query += " ORDER BY date DESC"

	rows, err := db.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type TradeInfo struct {
		UID  string    `json:"uid"`
		Date time.Time `json:"date"`
	}

	var tradeInfos []TradeInfo
	for rows.Next() {
		var ti TradeInfo
		if err := rows.Scan(&ti.UID, &ti.Date); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		tradeInfos = append(tradeInfos, ti)
	}

	c.JSON(http.StatusOK, tradeInfos)
}

func getTradeByUID(c *gin.Context) {
	uid := c.Param("uid")

	var trade TradeData
	var itemsJSON []byte

	err := db.QueryRow("SELECT uid, date, trader, recipient, items FROM trades WHERE uid = $1", uid).
		Scan(&trade.UID, &trade.Date, &trade.Trader, &trade.Recipient, &itemsJSON)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trade not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := json.Unmarshal(itemsJSON, &trade.Items); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to unmarshal items data"})
		return
	}

	c.JSON(http.StatusOK, trade)
}
