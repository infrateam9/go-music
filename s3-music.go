package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
)

const (
	CHARSET           = "UTF-8"
	MIN_SEARCH_STR    = 1
	MAX_SEARCH_RESULT = 100
	TXT_ACC_DIR       = "Server is unable to access the directory."
	TXT_NO_RES        = "Server not responding."
	TXT_MIN_SEARCH    = "Minimum search characters: "
)

var audioExtensions = []string{"mp3", "wav", "ogg", "mp4"}
var buildDate, commitHash, version string

// S3 configuration from environment variables
var (
	s3Bucket = os.Getenv("BUCKET")
	s3Region = os.Getenv("AWS_REGION")
	s3Prefix = os.Getenv("S3_PREFIX") // optional, e.g. "music/"
)

var s3Client *s3.Client

// responseWriter to capture the response for logging
type responseWriter struct {
	gin.ResponseWriter
	buffer *bytes.Buffer
}

// Write captures the response data
func (rw *responseWriter) Write(b []byte) (int, error) {
	rw.buffer.Write(b)                // Store the response
	return rw.ResponseWriter.Write(b) // Write the response to the original ResponseWriter
}

// logResponse logs the response
func logResponse(c *gin.Context, response string) {
	log.Printf("Response to %s %s: %s", c.Request.Method, c.Request.URL.Path, response)
}

// ResponseLogger middleware to log responses
func ResponseLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		var responseBuffer bytes.Buffer
		writer := &responseWriter{ResponseWriter: c.Writer, buffer: &responseBuffer}
		c.Writer = writer
		c.Next()
		statusCode := c.Writer.Status()
		if statusCode >= 400 {
			logResponse(c, responseBuffer.String())
			return
		}
	}
}

// isAudioFile checks if a filename has a supported audio extension
func isAudioFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	for _, audioExt := range audioExtensions {
		if ext == "."+audioExt {
			return true
		}
	}
	return false
}

// ea escapes and formats data for embedding in HTML/JS
func ea(varData []interface{}) string {
	res := ""
	for i, v := range varData {
		if i > 0 {
			res += ","
		}
		if arr, ok := v.([]string); ok {
			quotedArr := make([]string, len(arr))
			for j, item := range arr {
				quotedArr[j] = `"` + strings.ReplaceAll(item, `"`, `\\"`) + `"`
			}
			res += "[" + strings.Join(quotedArr, ",") + "]"
		} else {
			res += `"` + strings.ReplaceAll(v.(string), `"`, `\\"`) + `"`
		}
	}
	return "[" + res + "]"
}

// echoReqHtml sends an HTML response back to the client's iframe
func echoReqHtml(c *gin.Context, data []interface{}, funcName string) {
	c.Header("Content-Type", "text/html; charset="+CHARSET)
	c.String(http.StatusOK, `<!DOCTYPE html>
<html>
<head>
    <meta charset="`+CHARSET+`">
    <script>
        var dataContainer = `+ea(data)+`;
    </script>
</head>
<body onload="parent.`+funcName+`(dataContainer)">
</body>
</html>`)
}

func initS3() error {
	if s3Bucket == "" || s3Region == "" {
		return fmt.Errorf("BUCKET and AWS_REGION environment variables must be set")
	}
	// Ensure s3Prefix ends with '/' if not empty
	if s3Prefix != "" && !strings.HasSuffix(s3Prefix, "/") {
		s3Prefix += "/"
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(s3Region))
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	s3Client = s3.NewFromConfig(cfg)
	return nil
}

func s3List(prefix string, delimiter string) ([]string, []string, error) {
	// List S3 objects and common prefixes (directories)
	var dirs, files []string
	input := &s3.ListObjectsV2Input{
		Bucket:    aws.String(s3Bucket),
		Prefix:    aws.String(s3Prefix + prefix),
		Delimiter: aws.String(delimiter),
	}
	resp, err := s3Client.ListObjectsV2(context.Background(), input)
	if err != nil {
		return nil, nil, err
	}
	for _, cp := range resp.CommonPrefixes {
		name := strings.TrimPrefix(*cp.Prefix, s3Prefix+prefix)
		name = strings.TrimSuffix(name, "/")
		if name != "" {
			dirs = append(dirs, name)
		}
	}
	for _, obj := range resp.Contents {
		name := strings.TrimPrefix(*obj.Key, s3Prefix+prefix)
		if name != "" && !strings.Contains(name, "/") {
			files = append(files, name)
		}
	}
	return dirs, files, nil
}

func s3ListAllDirs() ([]string, error) {
	// Recursively list all directories in S3 bucket
	var allDirs []string
	var walk func(prefix string) error
	walk = func(prefix string) error {
		input := &s3.ListObjectsV2Input{
			Bucket:    aws.String(s3Bucket),
			Prefix:    aws.String(s3Prefix + prefix),
			Delimiter: aws.String("/"),
		}
		resp, err := s3Client.ListObjectsV2(context.Background(), input)
		if err != nil {
			return err
		}
		for _, cp := range resp.CommonPrefixes {
			name := strings.TrimPrefix(*cp.Prefix, s3Prefix)
			name = strings.TrimSuffix(name, "/")
			allDirs = append(allDirs, name)
			if err := walk(name + "/"); err != nil {
				return err
			}
		}
		return nil
	}
	allDirs = append(allDirs, "") // root
	if err := walk(""); err != nil {
		return nil, err
	}
	return allDirs, nil
}

func s3ListAllAudioFiles(prefix string) ([]string, error) {
	// Recursively list all audio files under prefix
	var allFiles []string
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s3Bucket),
		Prefix: aws.String(s3Prefix + prefix),
	}
	paginator := s3.NewListObjectsV2Paginator(s3Client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.Background())
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			if isAudioFile(*obj.Key) {
				name := strings.TrimPrefix(*obj.Key, s3Prefix)
				allFiles = append(allFiles, name)
			}
		}
	}
	return allFiles, nil
}

func s3SearchFiles(searchStr string) ([]string, error) {
	// List all audio files and filter by searchStr
	allFiles, err := s3ListAllAudioFiles("")
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, f := range allFiles {
		if strings.Contains(strings.ToLower(f), strings.ToLower(searchStr)) {
			matches = append(matches, f)
		}
	}
	return matches, nil
}

func s3SearchDirs(searchStr string) ([]string, error) {
	allDirs, err := s3ListAllDirs()
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, d := range allDirs {
		if strings.Contains(strings.ToLower(d), strings.ToLower(searchStr)) {
			matches = append(matches, d+"/")
		}
	}
	return matches, nil
}

func s3GetAudioFile(key string) (io.ReadCloser, int64, string, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3Prefix + key),
	}
	resp, err := s3Client.GetObject(context.Background(), input)
	if err != nil {
		return nil, 0, "", err
	}
	var size int64 = 0
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	return resp.Body, size, aws.ToString(resp.ContentType), nil
}

// --- HANDLERS ---
func handleDirRequest(c *gin.Context, dir string) {
	dirs, files, err := s3List(dir, "/")
	if err != nil {
		log.Printf("S3 list error: %v", err)
		echoReqHtml(c, []interface{}{"error", TXT_ACC_DIR, dir, []string{}}, "getBrowserData")
		return
	}
	sort.Strings(dirs)
	sort.Strings(files)
	echoReqHtml(c, []interface{}{"ok", dir, dirs, files}, "getBrowserData")
}

func handleSearchTitle(c *gin.Context, searchStr string) {
	searchStr = strings.TrimSpace(searchStr)
	if len(searchStr) < MIN_SEARCH_STR {
		echoReqHtml(c, []interface{}{"error", TXT_MIN_SEARCH + fmt.Sprintf("%d", MIN_SEARCH_STR), []string{}}, "getSearchTitle")
		return
	}
	titles, err := s3SearchFiles(searchStr)
	if err != nil {
		log.Printf("S3 search error: %v", err)
		echoReqHtml(c, []interface{}{"error", "S3 search error", []string{}}, "getSearchTitle")
		return
	}
	if len(titles) > MAX_SEARCH_RESULT {
		titles = titles[:MAX_SEARCH_RESULT]
	}
	sort.Strings(titles)
	echoReqHtml(c, []interface{}{"", titles}, "getSearchTitle")
}

func handleSearchDir(c *gin.Context, searchStr string) {
	searchStr = strings.TrimSpace(searchStr)
	if len(searchStr) < MIN_SEARCH_STR {
		echoReqHtml(c, []interface{}{"error", TXT_MIN_SEARCH + fmt.Sprintf("%d", MIN_SEARCH_STR), []string{}}, "getSearchDir")
		return
	}
	dirs, err := s3SearchDirs(searchStr)
	if err != nil {
		log.Printf("S3 search dir error: %v", err)
		echoReqHtml(c, []interface{}{"error", "S3 search dir error", []string{}}, "getSearchDir")
		return
	}
	if len(dirs) > MAX_SEARCH_RESULT {
		dirs = dirs[:MAX_SEARCH_RESULT]
	}
	sort.Strings(dirs)
	echoReqHtml(c, []interface{}{"", dirs}, "getSearchDir")
}

func handleGetAllMp3(c *gin.Context) {
	files, err := s3ListAllAudioFiles("")
	if err != nil {
		log.Printf("S3 get all mp3 error: %v", err)
		echoReqHtml(c, []interface{}{"error", "Failed to scan S3 bucket"}, "getAllMp3Data")
		return
	}
	sort.Strings(files)
	echoReqHtml(c, []interface{}{"ok", files}, "getAllMp3Data")
}

func handleGetAllDirs(c *gin.Context) {
	dirs, err := s3ListAllDirs()
	if err != nil {
		log.Printf("S3 get all dirs error: %v", err)
		echoReqHtml(c, []interface{}{"error", "Failed to scan S3 directories"}, "getAllDirsData")
		return
	}
	sort.Strings(dirs[1:]) // keep root at top
	echoReqHtml(c, []interface{}{"ok", dirs}, "getAllDirsData")
}

func handleGetAllMp3InDir(c *gin.Context, dir string) {
	files, err := s3ListAllAudioFiles(dir)
	if err != nil {
		log.Printf("S3 get all mp3 in dir error: %v", err)
		echoReqHtml(c, []interface{}{"error", "Failed to scan S3 directory"}, "getAllMp3Data")
		return
	}
	sort.Strings(files)
	echoReqHtml(c, []interface{}{"ok", files}, "getAllMp3Data")
}

func handleGetAllMp3InDirs(c *gin.Context, data string) {
	var selectedFolders []string
	err := json.Unmarshal([]byte(data), &selectedFolders)
	if err != nil {
		echoReqHtml(c, []interface{}{"error", "Invalid folder data"}, "getAllMp3Data")
		return
	}
	var allFiles []string
	for _, folder := range selectedFolders {
		files, err := s3ListAllAudioFiles(folder)
		if err != nil {
			log.Printf("S3 get all mp3 in dirs error: %v", err)
			continue
		}
		allFiles = append(allFiles, files...)
	}
	// Remove duplicates and sort
	uniqueFiles := make(map[string]bool)
	var finalFiles []string
	for _, file := range allFiles {
		if !uniqueFiles[file] {
			uniqueFiles[file] = true
			finalFiles = append(finalFiles, file)
		}
	}
	sort.Strings(finalFiles)
	echoReqHtml(c, []interface{}{"ok", finalFiles}, "getAllMp3Data")
}

func handleRequest(c *gin.Context) {
	funcType := c.PostForm("dffunc")
	data := c.PostForm("dfdata")

	switch funcType {
	case "dir":
		handleDirRequest(c, data)
	case "searchTitle":
		handleSearchTitle(c, data)
	case "searchDir":
		handleSearchDir(c, data)
	case "getAllMp3":
		handleGetAllMp3(c)
	case "getAllMp3InDir":
		handleGetAllMp3InDir(c, data)
	case "getAllMp3InDirs":
		handleGetAllMp3InDirs(c, data)
	case "getAllDirs":
		handleGetAllDirs(c)
	default:
		echoReqHtml(c, []interface{}{"error", "Unknown function"}, "default")
	}
}

// --- MAIN ---
func main() {
	if err := initS3(); err != nil {
		log.Fatalf("S3 init error: %v", err)
	}
	fmt.Println("go-music build date: ", buildDate)
	fmt.Println("go-music commit: ", commitHash)
	fmt.Println("go-music version: ", version)
	awsKey := os.Getenv("AWS_ACCESS_KEY_ID")
	awsSecret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	awsKeyPrefix := ""
	awsSecretPrefix := ""
	if len(awsKey) >= 4 {
		awsKeyPrefix = awsKey[:4]
	}
	if len(awsSecret) >= 4 {
		awsSecretPrefix = awsSecret[:4]
	}
	fmt.Println("AWS_ACCESS_KEY_ID (first 4):", awsKeyPrefix)
	fmt.Println("AWS_SECRET_ACCESS_KEY (first 4):", awsSecretPrefix)
	fmt.Println("BUCKET:", s3Bucket)
	fmt.Println("AWS_REGION:", s3Region)
	fmt.Println("S3_PREFIX:", s3Prefix)

	r := gin.Default()

	// --- Serve static files from the "static" directory ---
	r.Static("/static", "./static")
	r.GET("/", func(c *gin.Context) {
		c.File("./static/index.html")
	})

	r.Use(ResponseLogger())

	// API route
	r.POST("/api", handleRequest)

	// Serve audio files from S3
	r.GET("/audio/*path", func(c *gin.Context) {
		key := strings.TrimPrefix(c.Param("path"), "/")
		body, size, contentType, err := s3GetAudioFile(key)
		if err != nil {
			log.Printf("S3 audio error: %v", err)
			c.String(http.StatusNotFound, "Audio not found")
			return
		}
		defer body.Close()
		c.DataFromReader(http.StatusOK, size, contentType, body, nil)
	})

	r.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "Not found")
	})

	r.Run(":8080")
}
