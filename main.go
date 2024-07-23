package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
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

	var err error
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create the table if it doesn't exist
	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS scans (
            id SERIAL PRIMARY KEY,
            user_id TEXT,
            scan_type TEXT,
            timestamp TIMESTAMP,
            data JSONB
        )
    `)
	if err != nil {
		log.Fatal(err)
	}

	r := gin.Default()
	r.POST("/scan", recordScan)
	r.GET("/scans/:user_id", getUserScans)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	r.Run(":" + port)
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
