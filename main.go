package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

type ScanData struct {
	UserID    string          `json:"user_id"`
	ScanType  string          `json:"scan_type"` // "inventory" or "room"
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"` // Store the full scan data as JSON
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

	r := gin.Default()
	r.POST("/scan", authenticateAPIKey(recordScan))
	r.GET("/scans/:user_id", getUserScans)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Starting server on port %s", port)
	if err := r.Run("0.0.0.0:" + port); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
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

	rows, err := db.Query("SELECT scan_type, timestamp, data FROM scans WHERE user_id = $1", userID)
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
		scan.UserID = userID
		scan.Timestamp = timestamp
		scans = append(scans, scan)
	}

	c.JSON(http.StatusOK, scans)
}
