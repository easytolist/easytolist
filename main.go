package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/gorilla/sessions"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv" // email send package
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// DB is initialized in db.go.

var store = sessions.NewCookieStore([]byte("super-secret-key"))

// forgot password on login page smtp
var smtpConfig = struct {
	Host      string
	Port      string
	Username  string
	Password  string
	FromEmail string
}{
	Host:      "smtp.gmail.com",
	Port:      "587",
	Username:  os.Getenv("SMTP_USERNAME"),
	Password:  os.Getenv("SMTP_PASSWORD"),
	FromEmail: "easytolist5@gmail.com",
}

// ChatHub manages active WebSocket connections.
type ChatHub struct {
	connections map[int]*websocket.Conn // key: userID
	mutex       sync.Mutex
}

var hub = ChatHub{
	connections: make(map[int]*websocket.Conn),
}

// chatWebSocketHandler upgrades HTTP connection to WebSocket and listens for messages.
func chatWebSocketHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get sender name
	var senderName string
	err := DB.QueryRow("SELECT name FROM users WHERE id = $1", userID).Scan(&senderName)
	if err != nil {
		senderName = "Unknown"
		log.Printf("Failed to fetch sender name: %v", err)
	}

	// WebSocket upgrade
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	// Register connection
	hub.mutex.Lock()
	hub.connections[userID] = conn
	hub.mutex.Unlock()

	defer func() {
		hub.mutex.Lock()
		delete(hub.connections, userID)
		hub.mutex.Unlock()
		conn.Close()
	}()

	// Message handling loop
	for {
		var msg struct {
			AdID       int    `json:"ad_id"`
			ReceiverID int    `json:"receiver_id"`
			Message    string `json:"message"`
		}

		// Read message
		if err := conn.ReadJSON(&msg); err != nil {
			log.Println("WebSocket read error:", err)
			break
		}

		// Save to database and get message ID
		var messageID int
		err := DB.QueryRow(
			`INSERT INTO messages (ad_id, sender_id, receiver_id, message, created_at)
             VALUES ($1, $2, $3, $4, $5)
             RETURNING id`,
			msg.AdID, userID, msg.ReceiverID, msg.Message, time.Now(),
		).Scan(&messageID)

		if err != nil {
			log.Printf("DB insert error: %v", err)
			conn.WriteJSON(map[string]string{"error": "Failed to save message"})
			continue
		}

		// Prepare message
		outMsg := map[string]interface{}{
			"type":        "message",
			"id":          messageID,
			"ad_id":       msg.AdID,
			"sender_id":   userID,
			"sender_name": senderName,
			"message":     msg.Message,
			"timestamp":   time.Now().Format(time.RFC3339),
		}

		// Broadcast to all relevant connections
		hub.mutex.Lock()
		for uid, conn := range hub.connections {
			if uid == userID || uid == msg.ReceiverID {
				if err := conn.WriteJSON(outMsg); err != nil {
					log.Printf("Error sending to user %d: %v", uid, err)
				}
			}
		}
		hub.mutex.Unlock()
	}
}

// sendMessageHandler handles AJAX POST request to send a message.
func sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	// Improved session handling with error checking (Step 5)
	session, err := store.Get(r, "session")
	if err != nil {
		log.Printf("Session error: %v", err)
		http.Error(w, "Session error", http.StatusInternalServerError)
		return
	}

	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		log.Printf("Invalid session userID: %v", session.Values["userID"])
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var msg struct {
		AdID      int    `json:"ad_id"`
		OtherUser int    `json:"other_user"`
		Content   string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		log.Printf("Invalid payload error: %v", err)
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	if msg.AdID == 0 || msg.OtherUser == 0 || msg.Content == "" {
		http.Error(w, "Missing required fields", http.StatusBadRequest)
		return
	}

	// New log for message attempt (Step 2)
	log.Printf("Attempting to send message: %+v", msg)

	var messageID int
	var senderName string
	now := time.Now()

	// Single query to insert and get ID with improved error logging (Step 2)
	err = DB.QueryRow(
		`INSERT INTO messages (ad_id, sender_id, receiver_id, message, created_at)
         VALUES ($1, $2, $3, $4, $5)
         RETURNING id`,
		msg.AdID, userID, msg.OtherUser, msg.Content, now,
	).Scan(&messageID)

	if err != nil {
		log.Printf("Database error details: %v", err) // Updated error log
		http.Error(w, "Failed to save message: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// New success log (Step 2)
	log.Printf("Message saved successfully, ID: %d", messageID)

	// Get sender name separately
	err = DB.QueryRow("SELECT name FROM users WHERE id = $1", userID).Scan(&senderName)
	if err != nil {
		senderName = "Unknown"
		log.Printf("Failed to fetch sender name: %v", err)
	}

	// Prepare WebSocket message
	outMsg := map[string]interface{}{
		"type":        "message",
		"id":          messageID,
		"ad_id":       msg.AdID,
		"sender_id":   userID,
		"sender_name": senderName,
		"message":     msg.Content,
		"timestamp":   now.Format(time.RFC3339),
	}

	hub.mutex.Lock()
	defer hub.mutex.Unlock()

	// Send to both parties
	if senderConn, exists := hub.connections[userID]; exists {
		if err := senderConn.WriteJSON(outMsg); err != nil {
			log.Printf("WS send to sender error: %v", err)
		}
	}
	if receiverConn, exists := hub.connections[msg.OtherUser]; exists {
		if err := receiverConn.WriteJSON(outMsg); err != nil {
			log.Printf("WS send to receiver error: %v", err)
		}
	}

	// Prepare response
	response := map[string]interface{}{
		"success":    true,
		"messageID":  messageID,
		"senderID":   userID,
		"senderName": senderName,
		"adID":       msg.AdID,
		"message":    msg.Content,
		"timestamp":  now.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Response encode error: %v", err)
	}
}

// getConversationHandler returns full conversation history for given ad and other user.
func getConversationHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	adIDStr := r.URL.Query().Get("ad_id")
	otherUserStr := r.URL.Query().Get("other_user")
	adID, err := strconv.Atoi(adIDStr)
	if err != nil {
		http.Error(w, "Invalid ad_id", http.StatusBadRequest)
		return
	}
	otherUser, err := strconv.Atoi(otherUserStr)
	if err != nil {
		http.Error(w, "Invalid other_user", http.StatusBadRequest)
		return
	}
	query := `
        SELECT m.sender_id, u.name, m.message, m.created_at
        FROM messages m
        JOIN users u ON m.sender_id = u.id
        WHERE m.ad_id = $1 AND ((m.sender_id = $2 AND m.receiver_id = $3) OR (m.sender_id = $3 AND m.receiver_id = $2))
        ORDER BY m.created_at ASC
    `
	rows, err := DB.Query(query, adID, userID, otherUser)
	if err != nil {
		http.Error(w, "Failed to fetch conversation", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type ChatMessage struct {
		SenderID   int       `json:"senderID"`
		SenderName string    `json:"senderName"`
		Content    string    `json:"content"`
		Timestamp  time.Time `json:"timestamp"` // Add timestamp field
	}
	var messages []ChatMessage
	for rows.Next() {
		var senderID int
		var senderName, content string
		var ts time.Time // Change this line
		if err := rows.Scan(&senderID, &senderName, &content, &ts); err != nil {
			log.Printf("Message scan error: %v", err)
			continue
		}
		messages = append(messages, ChatMessage{
			SenderID:   senderID,
			SenderName: senderName,
			Content:    content,
			Timestamp:  ts, // Add timestamp
		})
	}
	response := map[string]interface{}{
		"currentUserID": userID,
		"messages":      messages,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// userStatusHandler returns online status for a given user.
func userStatusHandler(w http.ResponseWriter, r *http.Request) {
	userIDStr := r.URL.Query().Get("user_id")
	if userIDStr == "" {
		http.Error(w, "user_id required", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(userIDStr)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	hub.mutex.Lock()
	_, online := hub.connections[id]
	hub.mutex.Unlock()
	response := map[string]bool{"online": online}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// messagesHandler fetches a conversation summary for the logged-in user.
// messagesHandler
func messagesHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	query := `
    SELECT 
        m.ad_id, 
        COALESCE(a.title, 'Deleted Ad') as ad_title,
        a.price,
        a.city,
        a.state,
        (SELECT STRING_AGG(ai.image_path, ',') FROM ad_images ai WHERE ai.ad_id = m.ad_id) as images,
        CASE WHEN m.sender_id = $1 THEN m.receiver_id ELSE m.sender_id END AS other_user,
        MAX(m.created_at) as last_msg_time
    FROM messages m
    LEFT JOIN ads a ON m.ad_id = a.id
    WHERE m.sender_id = $1 OR m.receiver_id = $1
    GROUP BY m.ad_id, a.title, a.price, a.city, a.state, other_user
    ORDER BY last_msg_time DESC;`

	rows, err := DB.Query(query, userID)
	if err != nil {
		log.Printf("Database query error: %v", err)
		http.Error(w, "Failed to fetch messages", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Conversation struct {
		AdID            int
		AdTitle         string
		AdPrice         float64
		AdCity          string
		AdState         string
		AdImages        []string
		OtherUserID     int
		LastMessageTime time.Time
	}

	var conversations []Conversation
	for rows.Next() {
		var conv Conversation
		var images string

		// सभी columns को सही क्रम में scan करें
		err := rows.Scan(
			&conv.AdID,
			&conv.AdTitle,
			&conv.AdPrice,
			&conv.AdCity,
			&conv.AdState,
			&images,
			&conv.OtherUserID,
			&conv.LastMessageTime,
		)
		if err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}

		if images != "" {
			conv.AdImages = strings.Split(images, ",")
		}

		conversations = append(conversations, conv)
	}

	renderTemplate(w, "account_messages", map[string]interface{}{
		"IsLoggedIn":    true,
		"Conversations": conversations,
	})
}

// accountHandler renders the account page with user info, ads and conversation history.
func accountHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Fetch user info
	var user struct {
		ID    int
		Name  string
		Email string
	}
	err := DB.QueryRow("SELECT id, name, email FROM users WHERE id = $1", userID).
		Scan(&user.ID, &user.Name, &user.Email)
	if err != nil {
		log.Printf("User query error: %v", err)
		http.Error(w, "User not found", http.StatusInternalServerError)
		return
	}

	// Fetch ads posted by user
	adQuery := `
SELECT
	a.id,
	a.title,
	a.description,
	COALESCE(STRING_AGG(ai.image_path, ','), '') AS images
FROM ads a
LEFT JOIN ad_images ai ON a.id = ai.ad_id
WHERE a.user_id = $1
GROUP BY a.id
ORDER BY a.id DESC`
	rows, err := DB.Query(adQuery, userID)
	if err != nil {
		log.Printf("Ads query error: %v", err)
		http.Error(w, "Error fetching ads", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Ad struct {
		ID          int
		Title       string
		Description string
		Images      []string
	}
	var ads []Ad
	for rows.Next() {
		var ad Ad
		var images string
		if err := rows.Scan(&ad.ID, &ad.Title, &ad.Description, &images); err != nil {
			log.Printf("Ad scan error: %v", err)
			continue
		}
		if images != "" {
			ad.Images = strings.Split(images, ",")
		}
		ads = append(ads, ad)
	}

	// Fetch conversations
	convQuery := `
SELECT
    m.ad_id,
    COALESCE(a.title, 'Deleted Ad') as ad_title,
    a.price,
    a.city,
    a.state,
    (SELECT STRING_AGG(ai.image_path, ',') FROM ad_images ai WHERE ai.ad_id = m.ad_id) as images,
    CASE WHEN m.sender_id = $1 THEN m.receiver_id ELSE m.sender_id END AS other_user,
    MAX(m.created_at) as last_msg_time
FROM messages m
LEFT JOIN ads a ON m.ad_id = a.id
WHERE m.sender_id = $1 OR m.receiver_id = $1
GROUP BY m.ad_id, a.title, a.price, a.city, a.state, other_user
ORDER BY last_msg_time DESC;`

	convRows, err := DB.Query(convQuery, userID)
	if err != nil {
		log.Printf("Conversation query error: %v", err)
		http.Error(w, "Error loading messages", http.StatusInternalServerError)
		return
	}
	defer convRows.Close()

	type Conversation struct {
		AdID            int
		AdTitle         string
		AdPrice         float64
		AdCity          string
		AdState         string
		AdImages        []string
		OtherUserID     int
		LastMessageTime time.Time
		Messages        []struct {
			SenderName string
			Content    string
		}
	}

	var conversations []Conversation
	for convRows.Next() {
		var conv Conversation
		var images string
		err := convRows.Scan(
			&conv.AdID,
			&conv.AdTitle,
			&conv.AdPrice,
			&conv.AdCity,
			&conv.AdState,
			&images,
			&conv.OtherUserID,
			&conv.LastMessageTime,
		)
		if err != nil {
			log.Printf("Conversation scan error: %v", err)
			continue
		}
		if images != "" {
			conv.AdImages = strings.Split(images, ",")
		}

		// Fetch messages
		msgQuery := `
SELECT m.sender_id, u.name, m.message, m.created_at
FROM messages m
JOIN users u ON m.sender_id = u.id
WHERE m.ad_id = $1 AND ((m.sender_id = $2 AND m.receiver_id = $3) OR (m.sender_id = $3 AND m.receiver_id = $2))
ORDER BY m.created_at ASC`
		msgRows, err := DB.Query(msgQuery, conv.AdID, userID, conv.OtherUserID)
		if err != nil {
			log.Printf("Message query error: %v", err)
			continue
		}
		defer msgRows.Close()

		for msgRows.Next() {
			var senderID int
			var senderName, content string
			var ts time.Time
			if err := msgRows.Scan(&senderID, &senderName, &content, &ts); err != nil {
				log.Printf("Message scan error: %v", err)
				continue
			}
			conv.Messages = append(conv.Messages, struct {
				SenderName string
				Content    string
			}{
				SenderName: senderName,
				Content:    content,
			})
		}
		conversations = append(conversations, conv)
	}

	// Prepare data
	data := struct {
		IsLoggedIn                 bool
		User                       interface{}
		Ads                        []Ad
		Conversations              []Conversation
		ShowSearchCategorySections bool // Add this flag
	}{
		IsLoggedIn:                 true,
		User:                       user,
		Ads:                        ads,
		Conversations:              conversations,
		ShowSearchCategorySections: false, // Set to false for account page
	}

	// Use common renderTemplate function
	renderTemplate(w, "account", data)
}

// editAdHandler processes the ad edit form submission.
func editAdHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Authentication: Get user ID from session
	session, _ := store.Get(r, "session") // Replace 'store' with your actual session store variable
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		log.Println("WARN: editAdHandler - Unauthorized access attempt.")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. Parse Form: Handle multipart form data including files
	// Increased max memory slightly just in case, adjust as needed
	err := r.ParseMultipartForm(20 << 20) // 20 MB max memory
	if err != nil {
		log.Printf("ERROR: editAdHandler - Failed to parse multipart form: %v", err)
		// Check if the error is because the request body is too large
		if err.Error() == "http: request body too large" {
			http.Error(w, "File upload is too large. Maximum size allowed is 20MB.", http.StatusBadRequest)
		} else {
			http.Error(w, "Error processing form data.", http.StatusBadRequest)
		}
		return
	}
	log.Printf("INFO: editAdHandler - User %d - Multipart form parsed.", userID)

	// 3. Extract Form Values
	adIDStr := r.FormValue("ad_id")
	title := r.FormValue("title")
	description := r.FormValue("description")
	deletedImagesRaw := r.FormValue("deleted_images") // Get raw string first
	log.Printf("INFO: editAdHandler - User %d - Received adID: %s, title: %s, deletedRaw: '%s'", userID, adIDStr, title, deletedImagesRaw)

	// Clean up deleted images list: handle empty string, trim spaces
	var deletedImages []string
	if deletedImagesRaw != "" {
		rawSplit := strings.Split(deletedImagesRaw, ",")
		for _, img := range rawSplit {
			trimmed := strings.TrimSpace(img)
			if trimmed != "" {
				deletedImages = append(deletedImages, trimmed)
			}
		}
	}
	log.Printf("INFO: editAdHandler - User %d - Parsed deleted images: %v", userID, deletedImages)

	// 4. Validate Ad ID
	adIDInt, err := strconv.Atoi(adIDStr)
	if err != nil {
		log.Printf("WARN: editAdHandler - User %d - Invalid ad ID format: %s", userID, adIDStr)
		http.Error(w, "Invalid ad ID", http.StatusBadRequest)
		return
	}

	// --- Start Database Transaction ---
	tx, err := DB.Begin() // Use your actual DB connection variable
	if err != nil {
		log.Printf("ERROR: editAdHandler - User %d - Failed to begin transaction: %v", userID, err)
		http.Error(w, "Database error, please try again later.", http.StatusInternalServerError)
		return
	}
	// Defer rollback in case of errors later in the function
	// It's safe to call Rollback even if Commit succeeded.
	defer tx.Rollback()
	log.Printf("INFO: editAdHandler - User %d - Transaction started for ad ID %d.", userID, adIDInt)

	// 5. Verify Ad Ownership (within the transaction)
	var adOwner int
	err = tx.QueryRow("SELECT user_id FROM ads WHERE id = $1 FOR UPDATE", adIDInt).Scan(&adOwner) // Add FOR UPDATE for locking within transaction
	if err != nil {
		if err == sql.ErrNoRows {
			log.Printf("WARN: editAdHandler - User %d - Ad not found: %d", userID, adIDInt)
			http.Error(w, "Ad not found", http.StatusNotFound)
		} else {
			log.Printf("ERROR: editAdHandler - User %d - Error checking ad ownership for ad %d: %v", userID, adIDInt, err)
			http.Error(w, "Database error", http.StatusInternalServerError)
		}
		return // Rollback will be deferred
	}
	if adOwner != userID {
		log.Printf("WARN: editAdHandler - User %d - Unauthorized attempt to edit ad %d owned by user %d", userID, adIDInt, adOwner)
		http.Error(w, "Unauthorized to edit this ad", http.StatusForbidden)
		return // Rollback will be deferred
	}
	log.Printf("INFO: editAdHandler - User %d - Verified ownership for ad ID %d.", userID, adIDInt)

	// 6. Update Ad Text Details (within the transaction)
	_, err = tx.Exec(
		"UPDATE ads SET title = $1, description = $2 WHERE id = $3 AND user_id = $4", // Added user_id check again for safety
		title, description, adIDInt, userID,
	)
	if err != nil {
		log.Printf("ERROR: editAdHandler - User %d - Failed to update ad text for ad %d: %v", userID, adIDInt, err)
		http.Error(w, "Failed to update ad details", http.StatusInternalServerError)
		return // Rollback will be deferred
	}
	log.Printf("INFO: editAdHandler - User %d - Updated text details for ad ID %d.", userID, adIDInt)

	// 7. Delete Specified Images (within the transaction)
	if len(deletedImages) > 0 {
		stmtDel, err := tx.Prepare("DELETE FROM ad_images WHERE ad_id = $1 AND image_path = $2")
		if err != nil {
			log.Printf("ERROR: editAdHandler - User %d - Failed to prepare delete statement for ad %d: %v", userID, adIDInt, err)
			http.Error(w, "Database error during image deletion preparation", http.StatusInternalServerError)
			return // Rollback will be deferred
		}
		defer stmtDel.Close()

		for _, imgToDelete := range deletedImages {
			log.Printf("INFO: editAdHandler - User %d - Attempting to delete image record: %s for ad %d", userID, imgToDelete, adIDInt)
			res, err := stmtDel.Exec(adIDInt, imgToDelete)
			if err != nil {
				// Log error but continue, maybe the image was already deleted or never existed for this ad
				log.Printf("WARN: editAdHandler - User %d - Failed to execute delete statement for image %s, ad %d: %v", userID, imgToDelete, adIDInt, err)
				continue // Try deleting next image
			}

			rowsAffected, _ := res.RowsAffected()
			if rowsAffected > 0 {
				log.Printf("INFO: editAdHandler - User %d - Deleted image record: %s for ad %d", userID, imgToDelete, adIDInt)
				// Now attempt to delete from filesystem ONLY if DB record was deleted
				filePathToDelete := filepath.Join("uploads", imgToDelete)
				err := os.Remove(filePathToDelete)
				if err != nil && !os.IsNotExist(err) {
					// Log error if deletion failed and the file wasn't already gone
					log.Printf("ERROR: editAdHandler - User %d - Failed to delete image file %s from filesystem: %v", userID, filePathToDelete, err)
					// Continue might be acceptable, but log it. Don't rollback the whole transaction for a file system error.
				} else {
					log.Printf("INFO: editAdHandler - User %d - Deleted image file: %s from filesystem", userID, filePathToDelete)
				}
			} else {
				log.Printf("INFO: editAdHandler - User %d - Image record %s for ad %d not found for deletion in DB (might have been deleted already).", userID, imgToDelete, adIDInt)
			}
		}
	}

	// 8. Upload and Save New Images (within the transaction)
	files := r.MultipartForm.File["images"] // Get slice of file headers
	log.Printf("INFO: editAdHandler - User %d - Received %d new files for upload for ad %d.", userID, len(files), adIDInt)

	// Prepare insert statement once before the loop
	stmtIns, err := tx.Prepare("INSERT INTO ad_images (ad_id, image_path) VALUES ($1, $2)")
	if err != nil {
		log.Printf("ERROR: editAdHandler - User %d - Failed to prepare insert statement for ad %d: %v", userID, adIDInt, err)
		http.Error(w, "Database error during image insertion preparation", http.StatusInternalServerError)
		return // Rollback will be deferred
	}
	defer stmtIns.Close() // Close prepared statement when function exits

	var successfullyUploadedPaths []string // Keep track of files saved in this request

	for _, fileHeader := range files {
		if fileHeader == nil {
			continue // Should not happen, but safe check
		}
		log.Printf("INFO: editAdHandler - User %d - Processing uploaded file: %s (Size: %d)", userID, fileHeader.Filename, fileHeader.Size)

		// Open the uploaded file content
		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("ERROR: editAdHandler - User %d - Failed to open uploaded file %s: %v", userID, fileHeader.Filename, err)
			continue // Skip this file, proceed to the next
		}
		// --- Explicit Close ---
		// No defer here, close manually after use or error

		// Generate a safe and unique filename (UUID + original extension)
		originalFilename := fileHeader.Filename
		originalExtension := filepath.Ext(originalFilename)
		uniqueFilename := uuid.New().String() + originalExtension // Safe filename
		filePath := filepath.Join("uploads", uniqueFilename)

		// Ensure the uploads directory exists
		// Consider doing this once outside the handler if performance is critical
		if err := os.MkdirAll("uploads", os.ModePerm); err != nil {
			log.Printf("ERROR: editAdHandler - User %d - Failed to create uploads directory: %v", userID, err)
			file.Close() // Close the opened uploaded file
			http.Error(w, "Server error creating upload directory", http.StatusInternalServerError)
			// No need to return here if we want to try other files, but likely a fatal server issue
			// Depending on desired behavior, you might want to return and rollback. Let's continue for now.
			continue
		}

		// Create the destination file on the filesystem
		dst, err := os.Create(filePath)
		if err != nil {
			log.Printf("ERROR: editAdHandler - User %d - Failed to create destination file %s: %v", userID, filePath, err)
			file.Close() // Close the opened uploaded file
			continue     // Skip this file
		}
		// --- Explicit Close ---
		// No defer here, close manually after use or error

		// Copy the file content
		_, err = io.Copy(dst, file)

		// --- Close files immediately after copy (or error) ---
		file.Close()
		dst.Close() // VERY IMPORTANT TO CLOSE THE DESTINATION FILE HERE

		if err != nil {
			log.Printf("ERROR: editAdHandler - User %d - Failed to copy uploaded file content to %s: %v", userID, filePath, err)
			// Attempt to remove the partially created/empty file
			os.Remove(filePath)
			continue // Skip this file
		}

		log.Printf("INFO: editAdHandler - User %d - Successfully saved file to %s", userID, filePath)

		// Insert the image record into the database (using prepared statement)
		_, err = stmtIns.Exec(adIDInt, uniqueFilename)
		if err != nil {
			log.Printf("ERROR: editAdHandler - User %d - Failed to insert image record %s into DB for ad %d: %v", userID, uniqueFilename, adIDInt, err)
			// CRITICAL: DB insert failed. Remove the file we just saved.
			os.Remove(filePath)
			// Stop processing further files? Or just skip this one?
			// For consistency, let's skip this one and continue, but log prominently.
			// If this were critical, you might `return` here to trigger the rollback.
			continue
		}

		log.Printf("INFO: editAdHandler - User %d - Successfully inserted image record %s into DB for ad %d", userID, uniqueFilename, adIDInt)
		successfullyUploadedPaths = append(successfullyUploadedPaths, uniqueFilename) // Track success
	} // End of loop for new files

	// Check if total images exceed limit (optional, depends on your rules)
	var currentImageCount int
	err = tx.QueryRow("SELECT COUNT(*) FROM ad_images WHERE ad_id = $1", adIDInt).Scan(currentImageCount)
	if err != nil {
		log.Printf("ERROR: editAdHandler - User %d - Failed to count images for ad %d after update: %v", userID, adIDInt, err)
		// Don't necessarily fail the whole request, but log it.
	} else {
		log.Printf("INFO: editAdHandler - User %d - Ad %d now has %d images.", userID, adIDInt, currentImageCount)
		// Example limit check:
		// maxImages := 5
		// if currentImageCount > maxImages {
		//    log.Printf("WARN: editAdHandler - User %d - Ad %d exceeds image limit (%d > %d). Rolling back.", userID, adIDInt, currentImageCount, maxImages)
		//    // You might choose to rollback here by returning an error
		//    http.Error(w, "Image limit exceeded after upload.", http.StatusBadRequest)
		//    return // Rollback will be deferred
		// }
	}

	// 9. Commit Transaction
	err = tx.Commit()
	if err != nil {
		log.Printf("ERROR: editAdHandler - User %d - Failed to commit transaction for ad %d: %v", userID, adIDInt, err)
		// If commit fails, try to clean up files saved *in this request*
		for _, uploadedPath := range successfullyUploadedPaths {
			os.Remove(filepath.Join("uploads", uploadedPath))
			log.Printf("INFO: editAdHandler - User %d - Cleaned up file %s due to commit failure.", userID, uploadedPath)
		}
		http.Error(w, "Failed to save changes due to a database error.", http.StatusInternalServerError)
		return
	}

	log.Printf("INFO: editAdHandler - User %d - Successfully committed transaction for ad %d.", userID, adIDInt)

	// 10. Success Redirect
	// Redirect to the account page, perhaps to the manage ads section
	http.Redirect(w, r, "/account#manage-ads", http.StatusSeeOther)
}

// deleteAdHandler handles ad deletion.
func deleteAdHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Check user session
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. Parse form data
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	adIDStr := r.FormValue("ad_id")
	adID, err := strconv.Atoi(adIDStr)
	if err != nil {
		http.Error(w, "Invalid ad ID", http.StatusBadRequest)
		return
	}

	// 3. Verify ad ownership
	var adOwner int
	err = DB.QueryRow("SELECT user_id FROM ads WHERE id = $1", adID).Scan(&adOwner)
	if err != nil || adOwner != userID {
		http.Error(w, "Ad not found or unauthorized", http.StatusForbidden)
		return
	}

	// 4. Get image paths for deletion
	var imagePaths []string
	rows, err := DB.Query("SELECT image_path FROM ad_images WHERE ad_id = $1", adID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err == nil {
				imagePaths = append(imagePaths, path)
			}
		}
	}

	// 5. Start transaction to delete related data
	tx, err := DB.Begin()
	if err != nil {
		log.Printf("Transaction begin error: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// Delete messages
	_, err = tx.Exec("DELETE FROM messages WHERE ad_id = $1", adID)
	if err != nil {
		log.Printf("Delete messages error: %v", err)
		http.Error(w, "Failed to delete messages", http.StatusInternalServerError)
		return
	}

	// Delete ad images from database
	_, err = tx.Exec("DELETE FROM ad_images WHERE ad_id = $1", adID)
	if err != nil {
		log.Printf("Delete ad_images error: %v", err)
		http.Error(w, "Failed to delete images", http.StatusInternalServerError)
		return
	}

	// Delete the ad
	_, err = tx.Exec("DELETE FROM ads WHERE id = $1", adID)
	if err != nil {
		log.Printf("Delete ad error: %v", err)
		http.Error(w, "Failed to delete ad", http.StatusInternalServerError)
		return
	}

	// Commit transaction
	if err = tx.Commit(); err != nil {
		log.Printf("Commit error: %v", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// 6. Delete image files from server
	for _, path := range imagePaths {
		if err := os.Remove(filepath.Join("uploads", path)); err != nil {
			log.Printf("Failed to delete image %s: %v", path, err)
		}
	}

	// 7. Redirect to account page
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// Common template renderer (updated)
// renderTemplate renders HTML templates using a base layout and partials.
// renderTemplate renders HTML templates using a base layout and partials.
// renderTemplate renders HTML templates using a base layout and partials.
// renderTemplate renders HTML templates using a base layout and partials.
func renderTemplate(w http.ResponseWriter, tmplName string, data interface{}) {
	// 1. Define the paths
	baseLayout := filepath.ToSlash("templates/layouts/base.html")
	partialsPattern := filepath.ToSlash("templates/partials/*.html")
	pageTemplate := filepath.ToSlash(fmt.Sprintf("templates/%s.html", tmplName))

	// 2. Find partial files
	partialFiles, err := filepath.Glob(partialsPattern)
	if err != nil {
		log.Printf("Error finding partial templates (%s): %v", partialsPattern, err)
		http.Error(w, "Internal Server Error: Could not load page components (partials).", http.StatusInternalServerError)
		return
	}

	// 3. Create slice and NORMALIZE partial paths
	filesToParse := []string{baseLayout, pageTemplate}
	for _, p := range partialFiles {
		filesToParse = append(filesToParse, filepath.ToSlash(p))
	}

	log.Printf("Parsing template files (normalized): %v", filesToParse)

	// 4. Create template instance with functions
	t := template.New("base.html").Funcs(template.FuncMap{
		"formatPrice": formatPrice,
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "Just now"
			}
			return t.Format("02 Jan 2006 15:04")
		},
	})

	// 5. Parse all the normalized files together
	t, err = t.ParseFiles(filesToParse...)
	if err != nil {
		log.Printf("Template parsing error (parsing %s with base/partials): %v", pageTemplate, err)
		http.Error(w, "Internal Server Error: Failed to load page content (parse).", http.StatusInternalServerError)
		return
	}

	// 6. Execute the base template (which will call the block definitions)
	err = t.ExecuteTemplate(w, "base.html", data)
	if err != nil {
		log.Printf("Template execution error (executing base.html): %v", err)
	}
}

// formatPrice helper function (ensure it's available)
func formatPrice(price float64) string {
	// Format to 2 decimal places initially
	formatted := fmt.Sprintf("%.2f", price)
	// Remove trailing zeros and the decimal point if it becomes redundant
	if strings.Contains(formatted, ".") {
		formatted = strings.TrimRight(formatted, "0") // Remove trailing zeros
		formatted = strings.TrimRight(formatted, ".") // Remove trailing decimal point if present
	}
	// Basic comma separation (for India locale - might need more robust library for complex cases)
	parts := strings.Split(formatted, ".")
	integerPart := parts[0]
	decimalPart := ""
	if len(parts) > 1 {
		decimalPart = "." + parts[1]
	}

	l := len(integerPart)
	if l <= 3 {
		return integerPart + decimalPart // No separator needed
	}

	// Indian numbering system (crore, lakh, thousand)
	// Start from right: last 3 digits, then groups of 2
	lastThree := integerPart[l-3:]
	remaining := integerPart[:l-3]
	n := len(remaining)
	result := ""
	for i := n - 1; i >= 0; i-- {
		result = string(remaining[i]) + result
		// Add comma after every 2 digits (except for the very beginning)
		if (n-i)%2 == 0 && i != 0 {
			result = "," + result
		}
	}

	return result + "," + lastThree + decimalPart
}

// ================================================
// Make sure the rest of your main.go code remains
// (handlers, main function, db connection etc.

// signupHandler handles user signup.
func signupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		name := r.FormValue("name")
		email := r.FormValue("email")
		password := r.FormValue("password")

		// Prepare data map for potential re-rendering with errors/values
		data := make(map[string]interface{})
		data["SubmittedName"] = name   // Pre-fill name on error
		data["SubmittedEmail"] = email // Pre-fill email on error

		// --- >>> Add Password Length Validation <<< ---
		if len(password) < 8 {
			data["ErrorMessage"] = "Password must be at least 8 characters long."
			w.WriteHeader(http.StatusBadRequest) // 400 Bad Request for invalid input
			renderTemplate(w, "signup", data)    // Re-render signup page with error
			return                               // Stop processing
		}
		// --- >>> End of Password Length Validation <<< ---

		// Check if email already exists
		var count int
		err := DB.QueryRow("SELECT COUNT(*) FROM users WHERE email = $1", email).Scan(&count)
		if err != nil || count > 0 {
			http.Error(w, "Email already exists", http.StatusBadRequest)
			return
		}

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "Error encrypting password", http.StatusInternalServerError)
			return
		}

		// Insert user with is_verified = false
		var userID int
		err = DB.QueryRow(
			"INSERT INTO users (name, email, password, is_verified) VALUES ($1, $2, $3, $4) RETURNING id",
			name, email, string(hashedPassword), false,
		).Scan(&userID)
		if err != nil {
			http.Error(w, "Signup failed", http.StatusInternalServerError)
			return
		}

		// Generate verification token
		token := uuid.New().String()
		expiresAt := time.Now().Add(24 * time.Hour)

		_, err = DB.Exec(
			"INSERT INTO verification_tokens (user_id, token, expires_at) VALUES ($1, $2, $3)",
			userID, token, expiresAt,
		)
		if err != nil {
			http.Error(w, "Failed to create verification token", http.StatusInternalServerError)
			return
		}

		// Send verification email
		verificationLink := fmt.Sprintf("http://%s/verify/%s", r.Host, token)
		if err := sendVerificationEmail(email, verificationLink); err != nil {
			log.Printf("Failed to send verification email: %v", err)
		}

		// Redirect to login page with a status query parameter
		redirectURL := "/login?status=verify_pending"
		http.Redirect(w, r, redirectURL, http.StatusSeeOther)
		return
	}
	renderTemplate(w, "signup", nil)
}

func sendVerificationEmail(to, verificationLink string) error {
	auth := smtp.PlainAuth(
		"",
		"easytolist5@gmail.com",
		"vekqzjjlhuwijzsy",
		smtpConfig.Host,
	)

	msg := []byte(
		"To: " + to + "\r\n" +
			"Subject: Email Verification - EasyToList\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
			`<html><body style="font-family: Arial, sans-serif;">
            <h2 style="color: #2c3e50;">Email Verification Required</h2>
            <p>Please click the link below to verify your email address:</p>
            <a href="` + verificationLink + `" style="background-color: #3498db; color: white; padding: 10px 20px; text-decoration: none; border-radius: 5px;">Verify Email</a>
            <p style="margin-top: 20px; color: #7f8c8d;">This link will expire in 24 hours.</p>
        </body></html>`,
	)

	return smtp.SendMail(
		smtpConfig.Host+":"+smtpConfig.Port,
		auth,
		smtpConfig.FromEmail,
		[]string{to},
		msg,
	)
}

// verification email signup handler
func verificationHandler(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")

	// Check token validity
	var userID int
	err := DB.QueryRow(`
		SELECT user_id FROM verification_tokens 
		WHERE token = $1 AND expires_at > NOW()
	`, token).Scan(&userID)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Invalid or expired token", http.StatusBadRequest)
		} else {
			http.Error(w, "Database error", http.StatusInternalServerError)
		}
		return
	}

	// Mark user as verified
	_, err = DB.Exec("UPDATE users SET is_verified = TRUE WHERE id = $1", userID)
	if err != nil {
		http.Error(w, "Failed to verify user", http.StatusInternalServerError)
		return
	}

	// Delete used token
	_, err = DB.Exec("DELETE FROM verification_tokens WHERE token = $1", token)
	if err != nil {
		log.Println("Failed to delete verification token:", err)
	}

	// Redirect to the login page with a success status parameter
	redirectURL := "/login?status=verified"
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
	// Note: No 'return' is needed here as Redirect already handles writing
}

// loginHandler handles user login.
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		email := r.FormValue("email")
		password := r.FormValue("password")

		// Prepare data map for rendering, especially if there's an error
		data := make(map[string]interface{})
		// Pre-fill email field in case of error, so user doesn't have to retype it
		data["SubmittedEmail"] = email

		var user struct {
			ID         int
			Password   string
			IsVerified bool
		}

		err := DB.QueryRow(
			"SELECT id, password, is_verified FROM users WHERE email = $1",
			email,
		).Scan(&user.ID, &user.Password, &user.IsVerified)

		// Check for user not found or other DB errors
		if err != nil {
			// Check specifically for no rows found, treat as invalid credentials
			if err == sql.ErrNoRows { // Make sure you have "database/sql" imported
				data["ErrorMessage"] = "Invalid email or password."
			} else {
				// Log the actual database error for debugging
				log.Printf("Database error during login for email %s: %v", email, err)
				// Show a generic error to the user
				data["ErrorMessage"] = "An error occurred. Please try again."
			}
			// Set appropriate status code before rendering
			w.WriteHeader(http.StatusUnauthorized)
			renderTemplate(w, "login", data) // Re-render login page with error
			return
		}

		// Check password
		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
			data["ErrorMessage"] = "Invalid email or password."
			w.WriteHeader(http.StatusUnauthorized)
			renderTemplate(w, "login", data) // Re-render login page with error
			return
		}

		// Check email verification <<< --- THE KEY CHANGE IS HERE --- >>>
		if !user.IsVerified {
			data["ErrorMessage"] = "Please verify your email first." // Specific error message
			// StatusForbidden (403) is arguably more appropriate than Unauthorized (401) here
			w.WriteHeader(http.StatusForbidden)
			renderTemplate(w, "login", data) // Re-render login page with error
			return                           // Stop processing
		}

		// --- Login Success ---
		// Create session
		session, _ := store.Get(r, "session") // Add error handling for store.Get if needed
		session.Values["userID"] = user.ID
		err = session.Save(r, w) // Add error handling for session.Save
		if err != nil {
			log.Printf("Error saving session: %v", err)
			// Decide how to handle session save error, maybe show an error page/message
			http.Error(w, "Could not log you in. Please try again.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther) // Redirect to dashboard/home
		return
	}

	// --- Replace the existing GET handling block with this ---
	// --- GET Request Handling ---
	data := make(map[string]interface{}) // Prepare data map

	// Check the status query parameter
	status := r.URL.Query().Get("status")
	if status == "verify_pending" {
		// Message after signup, before verification
		data["InfoMessage"] = "Please verify your email to login."
	} else if status == "verified" {
		// Message after successful email verification
		data["SuccessMessage"] = "Email verified successfully! You can now login."
	}
	// Add any other data needed for the template for GET requests here

	renderTemplate(w, "login", data) // Render login page with potential message
}

// forgot password on login page all handlers.
// Forgot Password Page Handler
func forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "forgot_password", nil)
}

// Reset Password Page Handler
func resetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Invalid token", http.StatusBadRequest)
		return
	}

	// Verify token validity
	var isValid bool
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	err := DB.QueryRow(
		`SELECT EXISTS(
            SELECT 1 FROM password_reset_tokens 
            WHERE token_hash = $1 
            AND expires_at > NOW()
        )`, tokenHash).Scan(&isValid)

	if err != nil || !isValid {
		http.Error(w, "Invalid or expired token", http.StatusBadRequest)
		return
	}

	renderTemplate(w, "reset_password", map[string]interface{}{
		"Token": token,
	})
}

// API Handlers
func forgotPasswordHandler(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	var userID int
	err := DB.QueryRow("SELECT id FROM users WHERE email = $1", request.Email).Scan(&userID)
	if err != nil {
		// Prevent email enumeration
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "If the email exists, a reset link will be sent"})
		return
	}

	// Generate token
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}
	tokenString := base64.URLEncoding.EncodeToString(token)
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(tokenString)))

	// Store in DB
	_, err = DB.Exec(
		`INSERT INTO password_reset_tokens 
        (user_id, token_hash, expires_at) 
        VALUES ($1, $2, NOW() + INTERVAL '1 hour')`,
		userID, tokenHash)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	// Send email (simulated)
	resetLink := fmt.Sprintf("http://localhost:8080/reset-password?token=%s", tokenString)
	log.Printf("Password reset link for %s: %s", request.Email, resetLink)

	// send email code new
	if err := sendResetEmail(request.Email, resetLink); err != nil {
		log.Printf("Failed to send reset email: %v", err)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Reset link sent"})
}

func resetPasswordHandler(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Validate token
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(request.Token)))
	var userID int
	err := DB.QueryRow(
		`DELETE FROM password_reset_tokens 
        WHERE token_hash = $1 
        AND expires_at > NOW()
        RETURNING user_id`, tokenHash).Scan(&userID)

	if err != nil {
		http.Error(w, "Invalid or expired token", http.StatusBadRequest)
		return
	}

	// Update password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(request.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	_, err = DB.Exec("UPDATE users SET password = $1 WHERE id = $2", hashedPassword, userID)
	if err != nil {
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Password updated successfully"})
}

// sendresetemail , by this email send to user
func sendResetEmail(email, resetLink string) error {
	auth := smtp.PlainAuth(
		"",
		"easytolist5@gmail.com", // <-- यहाँ सही ईमेल डालें
		"vekqzjjlhuwijzsy",      // <-- यहाँ App Password डालें
		smtpConfig.Host,
	)

	to := []string{email}
	msg := []byte(
		"To: " + email + "\r\n" +
			"Subject: EasyToList Password Reset\r\n" +
			"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
			`<html><body style="font-family: Arial, sans-serif;">
            <h2 style="color: #2c3e50;">Password Reset Request</h2>
            <p>Click this link to reset your password:</p>
            <a href="` + resetLink + `" style="background-color: #3498db; color: white; padding: 10px 20px; text-decoration: none; border-radius: 5px;">Reset Password</a>
            <p style="margin-top: 20px; color: #7f8c8d;">This link will expire in 1 hour.</p>
        </body></html>`,
	)

	return smtp.SendMail(
		smtpConfig.Host+":"+smtpConfig.Port,
		auth,
		smtpConfig.FromEmail,
		to,
		msg,
	)
}

// logoutHandler logs the user out.
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	delete(session.Values, "userID")
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// homeHandler fetches ads for the home page.
func homeHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	query := `
SELECT
	a.id,
	a.title,
	a.price,
	a.city,
	a.state,
	a.category,
	a.subcategory,
	COALESCE(STRING_AGG(ai.image_path, ','), '') AS image_paths
FROM ads a
LEFT JOIN ad_images ai ON a.id = ai.ad_id
GROUP BY a.id, a.title, a.price, a.city, a.state, a.category, a.subcategory
ORDER BY a.id DESC
LIMIT 16

`
	rows, err := DB.Query(query)
	if err != nil {
		http.Error(w, "Error fetching ads: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type Ad struct {
		ID          int
		Title       string
		Price       float64
		City        string
		State       string
		Category    string
		Subcategory string
		ImagePaths  []string
	}
	var ads []Ad
	for rows.Next() {
		var ad Ad
		var imagePaths string
		err := rows.Scan(&ad.ID, &ad.Title, &ad.Price, &ad.City, &ad.State, &ad.Category, &ad.Subcategory, &imagePaths)
		if err != nil {
			http.Error(w, "Error scanning ads: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if imagePaths != "" {
			ad.ImagePaths = strings.Split(imagePaths, ",")
		} else {
			ad.ImagePaths = []string{}
		}
		ads = append(ads, ad)
	}
	data := struct {
		IsLoggedIn                 bool
		Ads                        []Ad
		ShowSearchCategorySections bool // Add this flag
	}{
		IsLoggedIn:                 ok && userID > 0,
		Ads:                        ads,
		ShowSearchCategorySections: true, // Set to true for index page
	}
	renderTemplate(w, "index", data)
}

const PageSize = 16 // <<< PAGINATION: Define page size (should match frontend PAGE_SIZE)

// ===================== FilteredAd Struct =====================
// Purana struct jismein match scores API response ke liye include nahi kiye gaye hain.
type FilteredAd struct {
	ID          int             `json:"id"`
	Title       string          `json:"title"`
	Price       float64         `json:"price"`
	City        string          `json:"city"`
	State       string          `json:"state"`
	Category    string          `json:"category"`
	Subcategory string          `json:"subcategory"`
	ImagePaths  []string        `json:"image_paths"`
	Lat         float64         `json:"lat"`
	Lng         float64         `json:"lng"`
	Distance    sql.NullFloat64 `json:"distance"`
	CreatedAt   time.Time       `json:"created_at"`
}

// ===================== Filter Ads Handler with Pagination =====================
func filterAdsHandler(w http.ResponseWriter, r *http.Request) {
	// Method check: POST required
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Parse form data
	if err := r.ParseForm(); err != nil {
		log.Printf("Error parsing form: %v", err)
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	// Extract and trim parameters:
	latStr := strings.TrimSpace(r.FormValue("lat"))
	lngStr := strings.TrimSpace(r.FormValue("lng"))
	search := strings.TrimSpace(r.FormValue("search"))
	subcategoryInput := strings.TrimSpace(r.FormValue("subcategory"))
	categoryInput := strings.TrimSpace(r.FormValue("category"))
	pageStr := strings.TrimSpace(r.FormValue("page")) // For pagination

	// 2. Validate location
	var userLat, userLng float64
	hasLocation := false
	if latStr != "" && lngStr != "" {
		var errLat, errLng error
		userLat, errLat = strconv.ParseFloat(latStr, 64)
		userLng, errLng = strconv.ParseFloat(lngStr, 64)
		if errLat == nil && errLng == nil {
			hasLocation = true
		} else {
			log.Printf("Invalid coordinates format: lat=%s (%v), lng=%s (%v)", latStr, errLat, lngStr, errLng)
		}
	}
	if !hasLocation {
		http.Error(w, "Valid location (latitude and longitude) is required", http.StatusBadRequest)
		return
	}

	// <<< PAGINATION: Parse page number; default to page 1 if missing/invalid.
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}
	offset := (page - 1) * PageSize

	// Define maximum search radius (50 km = 50000 meters)
	maxDistance := 50000.0

	// --- Build SQL Query ---
	// Note: Purane code ki tarah, yahan hum computed match scores use kar rahe hain bina extra filtering on subcategory/category.
	// Args: $1=userLng, $2=userLat, $3=search, $4=subcategory, $5=category, $6=maxDistance.
	args := []interface{}{userLng, userLat, search, subcategoryInput, categoryInput, maxDistance}

	// Build inner query: Select ads within 50km, compute distance and match scores.
	var innerBuilder strings.Builder
	innerBuilder.WriteString(`
SELECT 
    a.id,
    a.title,
    a.price,
    a.city,
    a.state,
    a.category,
    a.subcategory,
    COALESCE(STRING_AGG(ai.image_path, ','), '') AS image_paths,
    a.lat,
    a.lng,
    a.created_at,
    ST_Distance(
        ST_MakePoint(a.lng, a.lat)::geography,
        ST_MakePoint($1, $2)::geography
    ) AS distance,
    (a.title ILIKE '%' || $3 || '%')::integer AS title_match,
    (a.subcategory = $4)::integer AS subcategory_match,
    (a.category = $5)::integer AS category_match
FROM ads a
LEFT JOIN ad_images ai ON a.id = ai.ad_id
WHERE
    ST_DWithin(
        ST_MakePoint(a.lng, a.lat)::geography,
        ST_MakePoint($1, $2)::geography,
        $6
    )
GROUP BY 
    a.id, a.title, a.price, a.city, a.state, 
    a.category, a.subcategory, a.lat, a.lng, a.created_at
`)

	// Construct outer query with ordering and pagination.
	// Outer query orders by match scores, then distance and created_at.
	// Pagination placeholders: $7 for LIMIT, $8 for OFFSET.
	outerQuery := fmt.Sprintf(`
SELECT 
    sub.id,
    sub.title,
    sub.price,
    sub.city,
    sub.state,
    sub.category,
    sub.subcategory,
    sub.image_paths,
    sub.lat,
    sub.lng,
    sub.created_at,
    sub.distance
FROM (
    %s
) sub
ORDER BY 
    title_match DESC,
    subcategory_match DESC,
    category_match DESC,
    distance ASC,
    created_at DESC
LIMIT $7 OFFSET $8
`, innerBuilder.String())

	// Append pagination args: LIMIT and OFFSET.
	args = append(args, PageSize, offset)

	// Debug: Print final query and arguments.
	log.Printf("Final Query:\n%s", outerQuery)
	log.Printf("Query Args: %v", args)

	// Execute query.
	rows, err := DB.Query(outerQuery, args...)
	if err != nil {
		log.Printf("Query failed: %v\nQuery: %s\nArgs: %v", err, outerQuery, args)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// Process results.
	var ads []FilteredAd
	for rows.Next() {
		var ad FilteredAd
		var imagePaths string
		// Expecting 12 columns in outer query result.
		scanArgs := []interface{}{
			&ad.ID, &ad.Title, &ad.Price, &ad.City, &ad.State,
			&ad.Category, &ad.Subcategory, &imagePaths, &ad.Lat, &ad.Lng,
			&ad.CreatedAt, &ad.Distance,
		}
		if err := rows.Scan(scanArgs...); err != nil {
			log.Printf("Row scan error: %v", err)
			continue
		}
		if imagePaths != "" {
			ad.ImagePaths = strings.Split(imagePaths, ",")
		} else {
			ad.ImagePaths = []string{}
		}
		ads = append(ads, ad)
	}

	if err := rows.Err(); err != nil {
		log.Printf("Error iterating query results: %v", err)
	}

	log.Printf("Found %d ads for page %d with given filters.", len(ads), page)

	// Return JSON response.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(ads); err != nil {
		log.Printf("Error encoding results to JSON: %v", err)
	}
}

// ==============================================================
//
//	Handler to Fetch Ads by Category/Subcategory sorted by Distance
//
// ==============================================================
func fetchBySubcategoryHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Method Check (Prefer GET for this type of request)
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use GET.", http.StatusMethodNotAllowed)
		log.Printf("WARN: fetchBySubcategoryHandler called with method %s", r.Method)
		return
	}

	// 2. Parse Query Parameters
	queryParams := r.URL.Query()
	category := strings.TrimSpace(queryParams.Get("category"))
	subcategory := strings.TrimSpace(queryParams.Get("subcategory"))
	latStr := strings.TrimSpace(queryParams.Get("lat"))
	lngStr := strings.TrimSpace(queryParams.Get("lng"))
	pageStr := strings.TrimSpace(queryParams.Get("page"))

	log.Printf("INFO: fetchBySubcategoryHandler received params - category: %s, subcategory: %s, lat: %s, lng: %s, page: %s",
		category, subcategory, latStr, lngStr, pageStr)

	// 3. Validate Required Parameters
	if category == "" {
		http.Error(w, "Bad Request: 'category' parameter is required", http.StatusBadRequest)
		log.Printf("ERROR: fetchBySubcategoryHandler missing 'category' parameter")
		return
	}
	if subcategory == "" {
		http.Error(w, "Bad Request: 'subcategory' parameter is required", http.StatusBadRequest)
		log.Printf("ERROR: fetchBySubcategoryHandler missing 'subcategory' parameter")
		return
	}

	// 4. Validate Location (Latitude & Longitude) - Required for Distance Sorting
	var userLat, userLng float64
	hasLocation := false
	if latStr != "" && lngStr != "" {
		var errLat, errLng error
		userLat, errLat = strconv.ParseFloat(latStr, 64)
		userLng, errLng = strconv.ParseFloat(lngStr, 64)
		if errLat == nil && errLng == nil {
			// Basic sanity check for coordinate range (optional but good)
			if userLat >= -90 && userLat <= 90 && userLng >= -180 && userLng <= 180 {
				hasLocation = true
			} else {
				log.Printf("WARN: fetchBySubcategoryHandler received out-of-range coordinates: lat=%f, lng=%f", userLat, userLng)
			}
		} else {
			log.Printf("ERROR: fetchBySubcategoryHandler failed to parse coordinates: lat=%s (%v), lng=%s (%v)", latStr, errLat, lngStr, errLng)
		}
	}

	if !hasLocation {
		// Distance sorting is the primary goal, so location is essential here.
		http.Error(w, "Bad Request: Valid 'lat' and 'lng' parameters are required for distance sorting", http.StatusBadRequest)
		log.Printf("ERROR: fetchBySubcategoryHandler missing or invalid location parameters")
		return
	}

	// 5. Parse and Validate Page Number for Pagination
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		log.Printf("WARN: fetchBySubcategoryHandler received invalid page '%s', defaulting to page 1", pageStr)
		page = 1 // Default to page 1 if invalid or missing
	}
	offset := (page - 1) * PageSize // Calculate the offset for the SQL query

	// 6. Build the SQL Query
	//    - Filter STRICTLY by category and subcategory.
	//    - Calculate distance using PostGIS ST_Distance on geography type.
	//    - ORDER BY distance ASC (closest first), then by created_at DESC.
	//    - NO ST_DWithin filter (no maximum distance).
	//    - Use LIMIT and OFFSET for pagination.
	//    - Aggregate image paths correctly.

	query := `
SELECT
    a.id,
    a.title,
    a.price,
    a.city,
    a.state,
    a.category,
    a.subcategory,
    COALESCE(STRING_AGG(ai.image_path, ',' ORDER BY ai.id), '') AS image_paths, -- Ensure consistent image order
    a.lat,
    a.lng,
    a.created_at,
    -- Calculate distance in meters using geography type
    ST_Distance(
        ST_MakePoint(a.lng, a.lat)::geography,
        ST_MakePoint($1, $2)::geography  -- Use user's lng ($1) and lat ($2)
    ) AS distance
FROM ads a
LEFT JOIN ad_images ai ON a.id = ai.ad_id
WHERE
    a.category = $3 AND a.subcategory = $4 -- Strict filtering
GROUP BY
    -- Group by all non-aggregated fields from the 'ads' table
    a.id, a.title, a.price, a.city, a.state, a.category, a.subcategory,
    a.lat, a.lng, a.created_at
ORDER BY
    distance ASC,        -- Primary sort: Closest ads first
    a.created_at DESC    -- Secondary sort: Newest ads first among those at the same distance
LIMIT $5   -- PageSize
OFFSET $6  -- Calculated offset
`
	// Prepare arguments in the correct order for the placeholders ($1, $2, ...)
	args := []interface{}{userLng, userLat, category, subcategory, PageSize, offset}

	// 7. Log the Query and Arguments for Debugging
	log.Printf("DEBUG: Executing Subcategory Fetch Query:\n%s", query)
	log.Printf("DEBUG: Query Args: lng=$1=%f, lat=$2=%f, category=$3=%s, subcategory=$4=%s, LIMIT=$5=%d, OFFSET=$6=%d",
		userLng, userLat, category, subcategory, PageSize, offset)

	// 8. Execute the Query
	rows, err := DB.Query(query, args...) // Assumes DB is your *sql.DB connection pool
	if err != nil {
		log.Printf("ERROR: Subcategory query execution failed: %v", err)
		http.Error(w, "Internal Server Error: Failed to query database", http.StatusInternalServerError)
		return
	}
	defer rows.Close() // Ensure rows are closed when the function exits

	// 9. Process the Query Results
	var ads []FilteredAd // Slice to hold the results
	for rows.Next() {
		var ad FilteredAd
		var imagePaths sql.NullString // Use NullString for COALESCE results
		var distance sql.NullFloat64  // Use NullFloat64 for distance

		scanArgs := []interface{}{
			&ad.ID, &ad.Title, &ad.Price, &ad.City, &ad.State,
			&ad.Category, &ad.Subcategory,
			&imagePaths, // Scan into NullString
			&ad.Lat, &ad.Lng, &ad.CreatedAt,
			&distance, // Scan into NullFloat64
		}

		if err := rows.Scan(scanArgs...); err != nil {
			log.Printf("ERROR: Failed to scan row in fetchBySubcategoryHandler: %v", err)
			continue // Skip this row if scanning fails
		}

		// Process aggregated image paths
		if imagePaths.Valid && imagePaths.String != "" {
			ad.ImagePaths = strings.Split(imagePaths.String, ",")
		} else {
			ad.ImagePaths = []string{} // Ensure it's an empty slice, not nil
		}

		// Assign distance (sql.NullFloat64 handles JSON marshalling correctly)
		ad.Distance = distance

		// Append the processed ad to the results slice
		ads = append(ads, ad)
	}

	// Check for errors encountered during row iteration
	if err := rows.Err(); err != nil {
		log.Printf("ERROR: Error encountered during row iteration in fetchBySubcategoryHandler: %v", err)
		// Depending on severity, you might still return results found so far, or an error
		// For now, just log it.
	}

	log.Printf("INFO: Found %d ads for page %d (category: '%s', subcategory: '%s')", len(ads), page, category, subcategory)

	// 10. Return JSON Response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // Send OK status

	// Encode the ads slice directly into the response writer
	if err := json.NewEncoder(w).Encode(ads); err != nil {
		// Log error, but can't send HTTP error as header/status is already sent
		log.Printf("ERROR: Failed to encode results to JSON in fetchBySubcategoryHandler: %v", err)
	}
}

// Remember to register this handler in your main function or router setup:
// http.HandleFunc("/fetch-by-subcategory", fetchBySubcategoryHandler)

// ================= Pagination Code Start =================
// fetchAdsHandler handles paginated fetching of ads.
func fetchAdsHandler(w http.ResponseWriter, r *http.Request) {
	// Page number query parameter se lein, default page 1
	pageStr := r.URL.Query().Get("page")
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}
	limit := 16
	offset := (page - 1) * limit

	// Query to fetch ads with pagination
	query := `
    SELECT
        a.id, a.title, a.price, a.city, a.state, a.category, a.subcategory,
        COALESCE(STRING_AGG(ai.image_path, ','), '') AS image_paths
    FROM ads a
    LEFT JOIN ad_images ai ON a.id = ai.ad_id
    GROUP BY a.id, a.title, a.price, a.city, a.state, a.category, a.subcategory
    ORDER BY a.id DESC
    LIMIT $1 OFFSET $2
    `
	rows, err := DB.Query(query, limit, offset)
	if err != nil {
		http.Error(w, "Error fetching ads: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Ad struct {
		ID          int      `json:"id"`
		Title       string   `json:"title"`
		Price       float64  `json:"price"`
		City        string   `json:"city"`
		State       string   `json:"state"`
		Category    string   `json:"category"`
		Subcategory string   `json:"subcategory"`
		ImagePaths  []string `json:"image_paths"`
	}
	var ads []Ad
	for rows.Next() {
		var ad Ad
		var imagePaths string
		err := rows.Scan(&ad.ID, &ad.Title, &ad.Price, &ad.City, &ad.State, &ad.Category, &ad.Subcategory, &imagePaths)
		if err != nil {
			http.Error(w, "Error scanning ad: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if imagePaths != "" {
			ad.ImagePaths = strings.Split(imagePaths, ",")
		} else {
			ad.ImagePaths = []string{}
		}
		ads = append(ads, ad)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ads)
}

// ================= Pagination Code End =================
// =================postadhandler===========
func postAdHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	userID, ok := session.Values["userID"].(int)
	if !ok || userID <= 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if r.Method == "POST" {
		err := r.ParseMultipartForm(10 << 20) // 10 MB max
		if err != nil {
			http.Error(w, "File upload too large", http.StatusBadRequest)
			return
		}

		// Extract form values
		title := r.FormValue("title")
		category := r.FormValue("category")
		subcategory := r.FormValue("subcategory")
		description := r.FormValue("description")
		priceStr := r.FormValue("price")
		city := r.FormValue("city")
		state := r.FormValue("state")
		pincode := r.FormValue("pincode")
		submittedLat := r.FormValue("lat")
		submittedLng := r.FormValue("lng")
		isEdited := r.FormValue("isEdited") == "true"

		// Convert price
		price, err := strconv.ParseFloat(priceStr, 64)
		if err != nil {
			http.Error(w, "Invalid price", http.StatusBadRequest)
			return
		}

		var lat, lng float64
		var errGeocode error

		// Case 1: Use exact coordinates if available and not edited
		if !isEdited && submittedLat != "" && submittedLng != "" {
			lat, err = strconv.ParseFloat(submittedLat, 64)
			if err != nil {
				http.Error(w, "Invalid latitude", http.StatusBadRequest)
				return
			}
			lng, err = strconv.ParseFloat(submittedLng, 64)
			if err != nil {
				http.Error(w, "Invalid longitude", http.StatusBadRequest)
				return
			}
		} else {
			// Case 2 & 3: Geocode from city+pincode (edited or manual entry)
			lat, lng, errGeocode = geocodeCityPincode(city, pincode)
			if errGeocode != nil {
				// Fallback to pincode-only geocoding
				lat, lng, errGeocode = geocodePincode(pincode)
				if errGeocode != nil {
					http.Error(w, "Location lookup failed: "+errGeocode.Error(), http.StatusBadRequest)
					return
				}
			}
		}

		// Insert into database
		var adID int
		err = DB.QueryRow(`
            INSERT INTO ads 
                (user_id, title, category, subcategory, description, price, city, state, pincode, lat, lng)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
            RETURNING id`,
			userID, title, category, subcategory, description, price, city, state, pincode, lat, lng).Scan(&adID)
		if err != nil {
			http.Error(w, "Ad posting failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Handle image uploads
		files := r.MultipartForm.File["images"]
		for _, fileHeader := range files {
			if len(files) > 5 {
				break
			}
			file, err := fileHeader.Open()
			if err != nil {
				continue
			}
			defer file.Close()

			uniqueID := uuid.New()
			filename := fmt.Sprintf("%s-%s", uniqueID.String(), fileHeader.Filename)
			filePath := filepath.Join("uploads", filename)

			if _, err := os.Stat("uploads"); os.IsNotExist(err) {
				os.Mkdir("uploads", 0755)
			}

			dst, err := os.Create(filePath)
			if err != nil {
				continue
			}
			defer dst.Close()

			if _, err := io.Copy(dst, file); err != nil {
				continue
			}

			DB.Exec("INSERT INTO ad_images (ad_id, image_path) VALUES ($1, $2)", adID, filename)
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	renderTemplate(w, "post_ad", nil)
}

// geocodeHandler handles reverse geocoding (coordinates to address)
func geocodeHandler(w http.ResponseWriter, r *http.Request) {
	lat := r.URL.Query().Get("lat")
	lng := r.URL.Query().Get("lng")

	// Call Google Maps Reverse Geocoding API
	url := fmt.Sprintf("https://maps.googleapis.com/maps/api/geocode/json?latlng=%s,%s&key=%s", lat, lng, os.Getenv("GOOGLE_API_KEY"))
	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if result["status"] != "OK" {
		http.Error(w, "Geocoding failed", http.StatusBadRequest)
		return
	}

	// Parse address components
	addressComponents := result["results"].([]interface{})[0].(map[string]interface{})["address_components"].([]interface{})
	var city, state, pincode string
	for _, comp := range addressComponents {
		component := comp.(map[string]interface{})
		types := component["types"].([]interface{})
		for _, t := range types {
			typ := t.(string)
			switch typ {
			case "postal_code":
				pincode = component["short_name"].(string)
			case "locality":
				city = component["long_name"].(string)
			case "administrative_area_level_1":
				state = component["short_name"].(string)
			}
		}
	}

	// Return JSON response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"city":    city,
		"state":   state,
		"pincode": pincode,
		"status":  "OK",
	})
}

// geocodePincode converts pincode to lat/lng coordinates
func geocodeCityPincode(city, pincode string) (float64, float64, error) {
	// Combine city and pincode for more accurate geocoding
	address := fmt.Sprintf("%s, %s, India", city, pincode)
	url := fmt.Sprintf(
		"https://maps.googleapis.com/maps/api/geocode/json?address=%s&key=%s",
		url.QueryEscape(address),
		os.Getenv("GOOGLE_API_KEY"),
	)

	resp, err := http.Get(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, err
	}

	if result["status"] != "OK" {
		return 0, 0, fmt.Errorf("geocoding failed: %s", result["status"])
	}

	location := result["results"].([]interface{})[0].(map[string]interface{})["geometry"].(map[string]interface{})["location"].(map[string]interface{})
	lat := location["lat"].(float64)
	lng := location["lng"].(float64)
	return lat, lng, nil
}

// Geocode using only pincode (fallback option)
func geocodePincode(pincode string) (float64, float64, error) {
	url := fmt.Sprintf(
		"https://maps.googleapis.com/maps/api/geocode/json?address=%s&key=%s",
		url.QueryEscape(pincode),
		os.Getenv("GOOGLE_API_KEY"),
	)

	resp, err := http.Get(url)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, err
	}

	if result["status"] != "OK" {
		return 0, 0, fmt.Errorf("geocoding failed: %s", result["status"])
	}

	location := result["results"].([]interface{})[0].(map[string]interface{})["geometry"].(map[string]interface{})["location"].(map[string]interface{})
	lat := location["lat"].(float64)
	lng := location["lng"].(float64)
	return lat, lng, nil
}

// forwardGeocodeHandler: state और pincode से lat, lng निकालने के लिए
func forwardGeocodeHandler(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	pincode := r.URL.Query().Get("pincode")
	if state == "" || pincode == "" {
		http.Error(w, "State and pincode required", http.StatusBadRequest)
		return
	}
	lat, lng, err := geocodeCityPincode(state, pincode)
	if err != nil {
		http.Error(w, "Geocoding failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"lat": lat,
		"lng": lng,
	})
}

// aboutUsHandler "About Us" page ko render karta hai (using renderTemplate)
func aboutUsHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{}
	// renderTemplate ko call karein, jaisa dusre handlers kar rahe hain
	renderTemplate(w, "about", data) // Pass "about" as tmplName
}

// careerHandler renders the "Career" page.
func careerHandler(w http.ResponseWriter, r *http.Request) {
	// Data to pass to the template (empty for now)
	data := map[string]interface{}{}

	// It expects the template name without ".html" and assumes it's in "templates/"
	renderTemplate(w, "career", data)
}

// privacyPolicyHandler renders the "Privacy Policy" page.
func privacyPolicyHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{}
	// Use existing renderTemplate, passing the correct template name "privacy_policy"
	renderTemplate(w, "privacy_policy", data)
}

// bewareOfFraudsHandler renders the "Beware of Frauds" safety page.
func bewareOfFraudsHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{}
	// Use existing renderTemplate, passing the correct template name "beware_of_frauds"
	renderTemplate(w, "beware_of_frauds", data)
}

// submitContactHandler handles submissions from the footer contact form.
func submitContactHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Set content type to JSON for the response
	w.Header().Set("Content-Type", "application/json")

	// 2. Define a struct to hold the incoming JSON data
	var formData struct {
		Name    string `json:"name"`
		Email   string `json:"email"`
		Message string `json:"message"`
	}

	// 3. Decode the JSON request body into the struct
	err := json.NewDecoder(r.Body).Decode(&formData)
	if err != nil {
		log.Printf("ERROR: Decoding contact form JSON failed: %v", err)
		// Send a JSON error response back to the frontend
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid data format.",
		})
		return // Stop processing
	}

	// 4. Basic Validation (Server-side)
	if strings.TrimSpace(formData.Name) == "" || strings.TrimSpace(formData.Email) == "" || strings.TrimSpace(formData.Message) == "" {
		log.Printf("WARN: Contact form submission missing fields: Name=%s, Email=%s", formData.Name, formData.Email)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Please fill in all required fields.",
		})
		return // Stop processing
	}
	// You could add more robust email validation here if needed

	// 5. Insert data into the database
	insertQuery := `
        INSERT INTO contact_messages (name, email, message)
        VALUES ($1, $2, $3)
    `
	_, err = DB.Exec(insertQuery, formData.Name, formData.Email, formData.Message)
	if err != nil {
		log.Printf("ERROR: Failed to insert contact message into DB: %v", err)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Failed to send message. Please try again later.",
		})
		return // Stop processing
	}

	// 6. Log success and send success response
	log.Printf("INFO: Contact message received from %s (%s)", formData.Name, formData.Email)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

// adDetailHandler renders the ad detail page.
func adDetailHandler(w http.ResponseWriter, r *http.Request) {
	adID := chi.URLParam(r, "adID")
	id, err := strconv.Atoi(adID)
	if err != nil {
		http.Error(w, "Invalid ad ID", http.StatusBadRequest)
		return
	}
	var ad struct {
		ID          int
		Title       string
		Description string
		Price       float64
		City        string
		State       string
		Pincode     string
		Category    string
		Subcategory string
		SellerID    int
		Images      []string
	}
	err = DB.QueryRow(`
SELECT id, title, description, price, city, state, pincode, category, subcategory, user_id
FROM ads WHERE id = $1
`, id).Scan(&ad.ID, &ad.Title, &ad.Description, &ad.Price, &ad.City, &ad.State, &ad.Pincode, &ad.Category, &ad.Subcategory, &ad.SellerID)
	if err != nil {
		http.Error(w, "Ad not found", http.StatusNotFound)
		return
	}
	rows, err := DB.Query("SELECT image_path FROM ad_images WHERE ad_id = $1", id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var img string
			if err := rows.Scan(&img); err == nil {
				ad.Images = append(ad.Images, img)
			}
		}
	}
	renderTemplate(w, "ad_detail", map[string]interface{}{
		"Ad":                         ad,
		"IsLoggedIn":                 isLoggedIn(r),
		"Timestamp":                  time.Now().Unix(),
		"ShowSearchCategorySections": true, // Add this line to enable search and category sections

	})
}

func isLoggedIn(r *http.Request) bool {
	session, err := store.Get(r, "session")
	if err != nil {
		// Log the error for debugging, but treat as not logged in
		log.Printf("Session get error in isLoggedIn: %v", err) // Added logging
		return false
	}
	userID, ok := session.Values["userID"].(int)
	return ok && userID > 0
}

func main() {
	// Load .env file - primarily for local development.
	// Render uses environment variables set in its UI.
	err := godotenv.Load()
	if err != nil && !os.IsNotExist(err) {
		// Log error only if it's something other than "file not found"
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	// DB is initialized globally in db.go's init()
	// Ensure DB connection is closed when main function exits.
	// Call defer DB.Close() ONLY ONCE.
	defer DB.Close()

	// Setup Chi router
	router := chi.NewRouter()
	router.Use(middleware.Logger) // Add request logging

	// Serve static files and user uploads
	router.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	router.Handle("/uploads/*", http.StripPrefix("/uploads/", http.FileServer(http.Dir("./uploads"))))

	// --- Define Routes ONLY ONCE ---
	// Core routes
	router.Get("/", homeHandler)
	router.Get("/ad/{adID}", adDetailHandler)
	router.Get("/signup", signupHandler)
	router.Post("/signup", signupHandler)
	router.Get("/login", loginHandler)
	router.Post("/login", loginHandler)
	router.Get("/logout", logoutHandler)
	router.Get("/post-ad", postAdHandler)
	router.Post("/post-ad", postAdHandler)

	// Account routes
	router.Get("/account", accountHandler)
	router.Get("/account/messages", messagesHandler) // Specific messages page
	router.Post("/edit-ad", editAdHandler)
	router.Post("/delete-ad", deleteAdHandler)

	// API/AJAX routes
	router.Get("/fetch-ads", fetchAdsHandler)
	router.Get("/forward-geocode", forwardGeocodeHandler)
	router.Post("/filter-ads", filterAdsHandler)
	router.Get("/fetch-by-subcategory", fetchBySubcategoryHandler)
	router.Get("/geocode", geocodeHandler) // Reverse geocode

	// Verification routes
	router.Get("/verify/{token}", verificationHandler)

	// Chat routes
	router.Get("/ws", chatWebSocketHandler)
	router.Post("/send-message", sendMessageHandler)
	router.Get("/get-conversation", getConversationHandler)
	router.Get("/user-status", userStatusHandler)

	// footer links routers
	router.Get("/about-us", aboutUsHandler)
	router.Get("/career", careerHandler)
	router.Get("/privacy-policy", privacyPolicyHandler)
	router.Get("/beware-of-frauds", bewareOfFraudsHandler)
	router.Post("/submit-contact", submitContactHandler)

	// Password reset routes
	router.Get("/password/reset", forgotPasswordPage)
	router.Get("/reset-password", resetPasswordPage)
	router.Post("/api/forgot-password", forgotPasswordHandler)
	router.Post("/api/reset-password", resetPasswordHandler)
	// --- End of Route definitions ---

	// === Port Configuration (ONLY ONCE) ===
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Default for local development
		log.Printf("Defaulting to port %s for local development", port)
	}
	serverAddress := ":" + port
	// =====================================

	// === Start Server (ONLY ONCE) ===
	fmt.Printf("🚀 Server starting on %s\n", serverAddress)
	log.Fatal(http.ListenAndServe(serverAddress, router))
	// ==============================
}
