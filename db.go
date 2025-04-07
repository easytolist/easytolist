package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv" // Import strconv package

	_ "github.com/lib/pq"
)

var DB *sql.DB

func init() {
	ConnectDB()
}

func ConnectDB() {
	host := os.Getenv("DB_HOST")
	portStr := os.Getenv("DB_PORT") // Get port as string
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	dbname := os.Getenv("DB_NAME")

	// Convert port from string to integer
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("Error converting DB_PORT to integer: %v", err)
		return // Exit if port conversion fails
	}

	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)

	var dbErr error
	DB, dbErr = sql.Open("postgres", psqlInfo)
	if dbErr != nil {
		log.Fatal("⛔ Error connecting to database:", dbErr)
	}

	pingErr := DB.Ping()
	if pingErr != nil {
		log.Fatal("⛔ Database ping failed:", pingErr)
	}

	fmt.Println("✅ Database connected!")
}
