package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nfnt/resize"
)

const (
	uploadDir       = "uploaded-img"
	cleanupInterval = 5 * time.Minute
	unviewedExpiry  = 5 * time.Minute
	viewDuration    = 30 * time.Second
)

// Image represents the stored image data and its metadata.
type Image struct {
	ID            string
	OriginalPath  string
	MimeType      string
	UploadTime    time.Time
	FirstViewTime *time.Time
	mu            sync.Mutex
}

// store is the in-memory database for our images' metadata.
var store = struct {
	sync.RWMutex
	images map[string]*Image
}{images: make(map[string]*Image)}

// templates holds all our parsed HTML templates.
var templates = template.Must(template.ParseGlob("templates/*.html"))

// generateID creates a new random, unique ID for an image.
func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// homeHandler serves the main upload page.
func homeHandler(w http.ResponseWriter, r *http.Request) {
	err := templates.ExecuteTemplate(w, "upload.html", nil)
	if err != nil {
		http.Error(w, "Could not render template", http.StatusInternalServerError)
		log.Println("Error executing template:", err)
	}
}

// uploadHandler handles the image upload logic.
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024) // 10MB limit
	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Could not get uploaded file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read the file into a byte slice for processing
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Could not read image file", http.StatusInternalServerError)
		return
	}

	// Generate ID and save the original file
	id, err := generateID()
	if err != nil {
		http.Error(w, "Could not generate unique ID", http.StatusInternalServerError)
		return
	}
	originalPath := filepath.Join(uploadDir, fmt.Sprintf("%s-%s", id, header.Filename))
	if err := os.WriteFile(originalPath, fileBytes, 0644); err != nil {
		http.Error(w, "Could not save image file", http.StatusInternalServerError)
		return
	}

	// Generate thumbnail
	thumbnailData, err := createThumbnail(bytes.NewReader(fileBytes), header.Header.Get("Content-Type"))
	if err != nil {
		// If thumbnail fails, we can still proceed, just won't show a preview
		log.Println("Could not create thumbnail:", err)
		thumbnailData = "" // ensure it's an empty string
	}

	// Store image metadata
	image := &Image{
		ID:           id,
		OriginalPath: originalPath,
		MimeType:     header.Header.Get("Content-Type"),
		UploadTime:   time.Now(),
	}
	store.Lock()
	store.images[id] = image
	store.Unlock()

	// Prepare data for the share template
	shareLink := fmt.Sprintf("http://%s/view/%s", r.Host, id)
	data := map[string]string{
		"Link":      shareLink,
		"Thumbnail": thumbnailData,
	}

	// Render the share page
	err = templates.ExecuteTemplate(w, "view.html", data)
	if err != nil {
		http.Error(w, "Could not render template", http.StatusInternalServerError)
	}
}

// createThumbnail resizes an image and returns it as a Base64 string.
func createThumbnail(reader io.Reader, mimeType string) (string, error) {
	img, _, err := image.Decode(reader)
	if err != nil {
		return "", err
	}

	thumb := resize.Thumbnail(200, 200, img, resize.Lanczos3)

	var buf bytes.Buffer
	switch mimeType {
	case "image/png":
		err = png.Encode(&buf, thumb)
	case "image/jpeg":
		err = jpeg.Encode(&buf, thumb, nil)
	default:
		// Default to JPEG for other types like GIF
		err = jpeg.Encode(&buf, thumb, nil)
	}

	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// viewPageHandler serves the HTML page with the progress bar.
func viewPageHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/view/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	store.RLock()
	img, ok := store.images[id]
	store.RUnlock()

	if !ok {
		templates.ExecuteTemplate(w, "expired.html", nil)
		return
	}

	img.mu.Lock()
	defer img.mu.Unlock()

	// Set first view time if not already set
	if img.FirstViewTime == nil {
		now := time.Now()
		img.FirstViewTime = &now
		time.AfterFunc(viewDuration, func() {
			store.Lock()
			delete(store.images, id)
			os.Remove(img.OriginalPath) // Delete file from disk
			store.Unlock()
			log.Printf("Deleted image %s and file %s after timeout.", id, img.OriginalPath)
		})
	}

	// Check if link is expired
	if time.Since(*img.FirstViewTime) > viewDuration {
		templates.ExecuteTemplate(w, "expired.html", nil)
		return
	}

	// Serve the view page
	data := map[string]interface{}{
		"ImageURL":      fmt.Sprintf("/img/%s", id),
		"Duration":      viewDuration.Seconds(),
		"TimeRemaining": (viewDuration - time.Since(*img.FirstViewTime)).Seconds(),
	}
	templates.ExecuteTemplate(w, "view.html", data)
}

// serveImageHandler serves the actual image file.
func serveImageHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/img/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	store.RLock()
	img, ok := store.images[id]
	store.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	// Double-check expiry before serving the file
	img.mu.Lock()
	if img.FirstViewTime != nil && time.Since(*img.FirstViewTime) > viewDuration {
		img.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	img.mu.Unlock()

	http.ServeFile(w, r, img.OriginalPath)
}

// cleanupTask runs in the background to remove old, unviewed images.
func cleanupTask() {
	for {
		time.Sleep(cleanupInterval)
		store.Lock()
		for id, img := range store.images {
			if img.FirstViewTime == nil && time.Since(img.UploadTime) > unviewedExpiry {
				delete(store.images, id)
				err := os.Remove(img.OriginalPath)
				if err != nil {
					log.Printf("Failed to remove unviewed file %s: %v", img.OriginalPath, err)
				}
				log.Printf("Cleaned up unviewed image %s and file %s", id, img.OriginalPath)
			}
		}
		store.Unlock()
	}
}

func main() {
	// Create the upload directory if it doesn't exist.
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatalf("Failed to create upload directory: %v", err)
	}

	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/view/", viewPageHandler)
	http.HandleFunc("/img/", serveImageHandler)

	go cleanupTask()

	fmt.Println("Server starting on port 8080...")
	fmt.Printf("Uploaded images will be stored in the '%s' directory.\n", uploadDir)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
