package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"gopkg.in/gomail.v2"
)

type ScanData struct {
	UserID    string          `json:"user_id"`
	ScanType  string          `json:"scan_type"` // "inventory" or "room"
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"` // Store the full scan data as JSON
}

type TradeItem struct {
	UID       string    `json:"uid"`
	Date      time.Time `json:"date"`
	Trader    string    `json:"trader"`
	Recipient string    `json:"recipient"`
	ItemName  string    `json:"item_name"`
	ItemID    int       `json:"item_id"`
	HCValue   float64   `json:"hc_value"`
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
	r.GET("/trade", authenticateGetAPIKey(getAllTradeUIDs))
	r.GET("/trade/:uid", authenticateGetAPIKey(getTradeByUID))
	r.POST("/deletion-request", authenticateAPIKey(handleDeletionRequest))

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
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS trades (
			uid UUID,
			date TIMESTAMP,
			trader TEXT,
			recipient TEXT,
			item_name TEXT,
			item_id INTEGER,
			hc_value FLOAT
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

func authenticateGetAPIKey(f gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("Authorization")
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
			c.Abort()
			return
		}

		expectedAPIKey := os.Getenv("GET_API_KEY")
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
	var tradeItems []TradeItem
	if err := c.BindJSON(&tradeItems); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(tradeItems) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No trade items provided"})
		return
	}

	tradeUID := uuid.New().String()

	for _, item := range tradeItems {
		_, err := db.Exec(`
			INSERT INTO trades (uid, date, trader, recipient, item_name, item_id, hc_value)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, tradeUID, item.Date, item.Trader, item.Recipient, item.ItemName, item.ItemID, item.HCValue)

		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "recorded", "uid": tradeUID})
}

func getAllTradeUIDs(c *gin.Context) {
	// Parse query parameters for date filtering
	startDate := c.Query("start_date")
	endDate := c.Query("end_date")

	query := "SELECT DISTINCT uid, date FROM trades"
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

	rows, err := db.Query("SELECT date, trader, recipient, item_name, item_id, hc_value FROM trades WHERE uid = $1", uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var tradeItems []TradeItem
	for rows.Next() {
		var item TradeItem
		item.UID = uid
		if err := rows.Scan(&item.Date, &item.Trader, &item.Recipient, &item.ItemName, &item.ItemID, &item.HCValue); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		tradeItems = append(tradeItems, item)
	}

	if len(tradeItems) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Trade not found"})
		return
	}

	c.JSON(http.StatusOK, tradeItems)
}

func handleDeletionRequest(c *gin.Context) {
	var request struct {
		Username string `json:"username"`
		Reason   string `json:"reason"`
	}

	if err := c.BindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Log the request
	log.Printf("Deletion request received for user: %s", request.Username)

	// Send email notification
	if err := sendDeletionRequestEmail(request.Username, request.Reason); err != nil {
		log.Printf("Failed to send deletion request email: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process deletion request"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "Deletion request received and logged"})
}

func sendDeletionRequestEmail(username, reason string) error {
	m := gomail.NewMessage()
	m.SetHeader("From", os.Getenv("EMAIL_FROM"))
	m.SetHeader("To", os.Getenv("EMAIL_TO"))
	m.SetHeader("Subject", "Data Deletion Request")
	m.SetBody("text/plain", fmt.Sprintf("Username: %s\n\nReason: %s", username, reason))

	d := gomail.NewDialer("smtp.gmail.com", 587, os.Getenv("EMAIL_FROM"), os.Getenv("EMAIL_PASSWORD"))

	return d.DialAndSend(m)
}
