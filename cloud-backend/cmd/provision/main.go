package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/store"
)

func main() {
	kioskID := flag.String("kiosk-id", "", "Unique identifier for the kiosk (e.g., kiosk-library-01)")
	name := flag.String("name", "", "Human-readable label or location description for the machine")
	revoke := flag.Bool("revoke", false, "Deactivate the specified kiosk instead of provisioning a new secret")
	dbPath := flag.String("db", "", "Database URL or SQLite path (defaults to DATABASE_URL or XEROXGO_CLOUD_DB)")
	flag.Parse()

	cleanKioskID := strings.TrimSpace(*kioskID)
	if cleanKioskID == "" {
		fmt.Fprintln(os.Stderr, "Error: -kiosk-id is required.")
		flag.Usage()
		os.Exit(1)
	}

	targetDB := *dbPath
	if targetDB == "" {
		targetDB = os.Getenv("DATABASE_URL")
	}
	if targetDB == "" {
		targetDB = os.Getenv("XEROXGO_CLOUD_DB")
	}
	if targetDB == "" {
		targetDB = "./data/xeroxgo-cloud.db"
	}

	s, err := store.Open(targetDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to open store at %s: %v\n", targetDB, err)
		os.Exit(1)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if *revoke {
		existing, err := s.GetKiosk(ctx, cleanKioskID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error fetching kiosk: %v\n", err)
			os.Exit(1)
		}
		if existing == nil {
			fmt.Fprintf(os.Stderr, "Error: kiosk %q not found in registry.\n", cleanKioskID)
			os.Exit(1)
		}

		existing.IsActive = false
		existing.UpdatedAt = time.Now().UTC()
		if err := s.RegisterKiosk(ctx, *existing); err != nil {
			fmt.Fprintf(os.Stderr, "Error revoking kiosk: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Successfully deactivated kiosk %q. All authentication attempts will now be rejected with 403 Forbidden.\n", cleanKioskID)
		return
	}

	// Generate 32 bytes of cryptographic randomness for the kiosk secret
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating random secret: %v\n", err)
		os.Exit(1)
	}
	rawSecret := hex.EncodeToString(secretBytes)

	// Compute SHA-256 hash to persist
	h := sha256.Sum256([]byte(rawSecret))
	secretHash := hex.EncodeToString(h[:])

	kioskName := strings.TrimSpace(*name)
	if kioskName == "" {
		kioskName = cleanKioskID
	}

	now := time.Now().UTC()
	kioskRecord := store.Kiosk{
		KioskID:    cleanKioskID,
		SecretHash: secretHash,
		Name:       kioskName,
		IsActive:   true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.RegisterKiosk(ctx, kioskRecord); err != nil {
		fmt.Fprintf(os.Stderr, "Error registering kiosk: %v\n", err)
		os.Exit(1)
	}

	// Output provisioning information once to stdout
	fmt.Println("================================================================================")
	fmt.Println("                     XEROXGO KIOSK PROVISIONING SUCCESS                        ")
	fmt.Println("================================================================================")
	fmt.Printf("Kiosk ID:       %s\n", cleanKioskID)
	fmt.Printf("Name:           %s\n", kioskName)
	fmt.Printf("Status:         ACTIVE\n")
	fmt.Printf("Secret Hash:    %s\n", secretHash)
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Println("PROVISIONING CREDENTIAL (COPY NOW — THIS VALUE IS NEVER STORED OR SHOWN AGAIN):")
	fmt.Printf("XEROXGO_KIOSK_SECRET=%s\n", rawSecret)
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Println("OPERATOR INSTRUCTIONS:")
	fmt.Println("1. SSH into the physical kiosk device.")
	fmt.Println("2. Write the credential to /etc/xeroxgo/kiosk.env with restricted permissions:")
	fmt.Printf("   sudo tee /etc/xeroxgo/kiosk.env > /dev/null <<EOF\n")
	fmt.Printf("   XEROXGO_KIOSK_ID=%s\n", cleanKioskID)
	fmt.Printf("   XEROXGO_KIOSK_SECRET=%s\n", rawSecret)
	fmt.Printf("   EOF\n")
	fmt.Println("   sudo chmod 0600 /etc/xeroxgo/kiosk.env")
	fmt.Println("   sudo chown xeroxgo:xeroxgo /etc/xeroxgo/kiosk.env")
	fmt.Println("3. Restart the kiosk daemon: sudo systemctl restart xeroxgo-daemon")
	fmt.Println("================================================================================")
}
