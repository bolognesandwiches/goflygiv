package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/pkg/sftp"
	"github.com/robfig/cron/v3"
	"golang.org/x/crypto/ssh"
	"gopkg.in/gomail.v2"
)

type ScanData struct {
	UserID    string          `json:"user_id"`
	ScanType  string          `json:"scan_type"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
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

var privateKey []byte

func init() {
	privateKeyString := os.Getenv("SSH_PRIVATE_KEY")
	privateKey = []byte(privateKeyString)
	if len(privateKey) == 0 {
		log.Fatal("SSH_PRIVATE_KEY environment variable is not set or is empty")
	}
}
func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL environment variable is not set")
	}

	c := cron.New()
	_, err := c.AddFunc("0 1 * * *", performDailyExport)
	if err != nil {
		log.Fatalf("Error setting up cron job: %v", err)
	}
	c.Start()

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

	if err := db.Ping(); err != nil {
		log.Fatalf("Error connecting to the database: %v", err)
	}

	log.Println("Successfully connected to the database")

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
	r.POST("/export", authenticateAPIKey(handleExport))
	r.POST("/manual-export", authenticateAPIKey(handleManualExport))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Starting server on port %s", port)
	if err := r.Run("0.0.0.0:" + port); err != nil {
		log.Fatalf("Error starting server: %v", err)
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

func initDB() error {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS scans (
            id SERIAL PRIMARY KEY,
            user_id TEXT,
            scan_type TEXT,
            timestamp TIMESTAMP,
            data JSONB
        )
    `)
	if err != nil {
		return err
	}

	_, err = db.Exec(`
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
	if err != nil {
		return err
	}

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS deletion_requests (
            id SERIAL PRIMARY KEY,
            username TEXT NOT NULL,
            reason TEXT,
            request_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        )
    `)
	if err != nil {
		return err
	}

	return nil
}

func authenticateAPIKey(f gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader("Authorization")
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "API key required"})
			c.Abort()
			return
		}

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

func handleManualExport(c *gin.Context) {
	if err := executeExport(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Export failed: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Export completed successfully"})
}

func getUserScans(c *gin.Context) {
	userID := c.Param("user_id")

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
		scan.UserID = userID
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

	_, err := db.Exec("INSERT INTO deletion_requests (username, reason) VALUES ($1, $2)",
		request.Username, request.Reason)
	if err != nil {
		log.Printf("Error inserting deletion request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process deletion request"})
		return
	}

	log.Printf("Deletion request received for user: %s", request.Username)

	if err := sendDeletionRequestEmail(request.Username, request.Reason); err != nil {
		log.Printf("Failed to send deletion request email: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{"status": "Deletion request received and logged"})
}

func sendDeletionRequestEmail(username, reason string) error {
	m := gomail.NewMessage()
	m.SetHeader("From", os.Getenv("EMAIL_FROM"))
	m.SetHeader("To", os.Getenv("EMAIL_TO"))
	m.SetHeader("Subject", "Data Deletion Request")
	m.SetBody("text/plain", fmt.Sprintf("Username: %s\n\nReason: %s", username, reason))

	port, _ := strconv.Atoi(os.Getenv("SMTP_PORT"))
	d := gomail.NewDialer(os.Getenv("SMTP_HOST"), port, os.Getenv("EMAIL_FROM"), os.Getenv("EMAIL_PASSWORD"))

	return d.DialAndSend(m)
}

func handleExport(c *gin.Context) {
	filename, err := generateExportFile()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate export file"})
		return
	}

	err = uploadFileViaSFTP(filename)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to upload file via SFTP"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Export completed successfully"})
}
func generateExportFile() (string, error) {
	// Get today's date in the format YYYY-MM-DD
	today := time.Now().Format("2006-01-02")
	filename := fmt.Sprintf("trades_export_%s.csv", today)

	// Open the file for writing
	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	// Write CSV header
	_, err = file.WriteString("uid,date,trader,recipient,item_name,item_id,hc_value\n")
	if err != nil {
		return "", fmt.Errorf("failed to write CSV header: %v", err)
	}

	// Query the database for today's trades
	rows, err := db.Query(`
        SELECT uid, date, trader, recipient, item_name, item_id, hc_value 
        FROM trades 
        WHERE DATE(date) = CURRENT_DATE
    `)
	if err != nil {
		return "", fmt.Errorf("failed to query trades: %v", err)
	}
	defer rows.Close()

	// Write trade data to CSV
	for rows.Next() {
		var uid, trader, recipient, itemName string
		var date time.Time
		var itemID int
		var hcValue float64

		err := rows.Scan(&uid, &date, &trader, &recipient, &itemName, &itemID, &hcValue)
		if err != nil {
			return "", fmt.Errorf("failed to scan row: %v", err)
		}

		_, err = file.WriteString(fmt.Sprintf("%s,%s,%s,%s,%s,%d,%.2f\n",
			uid, date.Format("2006-01-02 15:04:05"), trader, recipient, itemName, itemID, hcValue))
		if err != nil {
			return "", fmt.Errorf("failed to write row: %v", err)
		}
	}

	return filename, nil
}

func performDailyExport() {
	if err := executeExport(); err != nil {
		log.Printf("Daily export failed: %v", err)
	}
}

func uploadFileViaSFTP(filename string) error {
	sftpHost := os.Getenv("SFTP_HOST")
	sftpPort := os.Getenv("SFTP_PORT")
	sftpUser := os.Getenv("SFTP_USER")

	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %v", err)
	}

	config := &ssh.ClientConfig{
		User: sftpUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	conn, err := ssh.Dial("tcp", fmt.Sprintf("%s:%s", sftpHost, sftpPort), config)
	if err != nil {
		return fmt.Errorf("failed to dial: %v", err)
	}
	defer conn.Close()

	client, err := sftp.NewClient(conn)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %v", err)
	}
	defer client.Close()

	localFile, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open local file: %v", err)
	}
	defer localFile.Close()

	remoteFilename := filepath.Base(filename)

	remoteFile, err := client.Create("/upload/" + remoteFilename)
	if err != nil {
		return fmt.Errorf("failed to create remote file: %v", err)
	}
	defer remoteFile.Close()

	_, err = io.Copy(remoteFile, localFile)
	if err != nil {
		return fmt.Errorf("failed to copy file contents: %v", err)
	}

	return nil
}

func executeExport() error {
	log.Println("Starting trade export")

	filename, err := generateExportFile()
	if err != nil {
		return fmt.Errorf("failed to generate export file: %v", err)
	}

	err = uploadFileViaSFTP(filename)
	if err != nil {
		return fmt.Errorf("failed to upload file via SFTP: %v", err)
	}

	log.Println("Trade export completed successfully")
	return nil
}
