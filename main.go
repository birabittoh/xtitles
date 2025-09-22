package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/joho/godotenv"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"gorm.io/gorm"
)

type Config struct {
	BaseURL        string
	Limit          int
	System         string
	DataDir        string
	PicturesFolder string
	PicturesSuffix string
	Address        string
	DBFile         string
}

type Response struct {
	Items []Title `json:"items"`
	Count int     `json:"count"`
}

type Title struct {
	TitleID         string    `json:"title_id" gorm:"primaryKey"`
	Name            string    `json:"name"`
	Systems         []string  `json:"systems" gorm:"serializer:json"`
	BingID          string    `json:"bing_id"`
	ServiceConfigID *string   `json:"service_config_id"`
	PFN             *string   `json:"pfn"`
	Pictures        []Picture `json:"pictures" gorm:"foreignKey:TitleID;references:TitleID"`
}

type Picture struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	TitleID string `json:"title_id" gorm:"index;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	Name    string `json:"name"`
}

type PaginatedResponse struct {
	Items  interface{} `json:"items"`
	Total  int64       `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
	Page   int         `json:"page"`
	Pages  int         `json:"pages"`
}

var db *gorm.DB
var config Config

func loadConfig() {
	// Load .env file if it exists
	godotenv.Load()

	config = Config{
		BaseURL:        getEnv("BASE_URL", "https://dbox.tools/api/title_ids/"),
		Limit:          getEnvInt("LIMIT", 100),
		System:         getEnv("SYSTEM", "XBOX360"),
		DataDir:        getEnv("DATA_DIR", "data"),
		PicturesFolder: getEnv("PICTURES_FOLDER", "titles"),
		PicturesSuffix: getEnv("PICTURES_SUFFIX", ".png"),
		Address:        getEnv("ADDRESS", "8081"),
		DBFile:         getEnv("DB_FILE", "titles.db"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func initDB() error {
	// Ensure data directory exists
	if err := os.MkdirAll(config.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(config.DataDir, config.DBFile)
	var err error
	db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	// Auto migrate the schema
	if err := db.AutoMigrate(&Title{}, &Picture{}); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	return nil
}

func fetchAllTitles() ([]Title, error) {
	var allTitles []Title
	offset := 0

	for {
		url := fmt.Sprintf("%s?system=%s&limit=%d&offset=%d", config.BaseURL, config.System, config.Limit, offset)
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
		}

		var r Response
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, fmt.Errorf("decode failed: %w", err)
		}

		allTitles = append(allTitles, r.Items...)

		fmt.Printf("Fetched %d titles (total: %d)\n", len(r.Items), len(allTitles))

		if len(r.Items) < config.Limit {
			break
		}
		offset += config.Limit
	}

	return allTitles, nil
}

func loadTitlesToDB() error {
	// Check if we already have data
	var count int64
	db.Model(&Title{}).Count(&count)
	if count > 0 {
		fmt.Printf("Database already contains %d titles\n", count)
		return nil
	}

	fmt.Println("Fetching titles from API...")
	titles, err := fetchAllTitles()
	if err != nil {
		return fmt.Errorf("fetching titles failed: %w", err)
	}

	// Process pictures from filesystem
	dirPngs, err := readPictureDirs()
	if err != nil {
		fmt.Printf("Warning: Error reading picture dirs: %v\n", err)
		dirPngs = make(map[string][]string)
	}

	// Insert titles into database
	fmt.Println("Inserting titles into database...")
	if err := db.CreateInBatches(titles, 100).Error; err != nil {
		return fmt.Errorf("inserting titles failed: %w", err)
	}

	// Insert pictures
	var allPictures []Picture
	for _, title := range titles {
		pngs := dirPngs[strings.ToLower(title.TitleID)]
		for _, png := range pngs {
			allPictures = append(allPictures, Picture{
				TitleID: title.TitleID,
				Name:    png,
			})
		}
	}

	if len(allPictures) > 0 {
		fmt.Println("Inserting pictures into database...")
		if err := db.CreateInBatches(allPictures, 100).Error; err != nil {
			return fmt.Errorf("inserting pictures failed: %w", err)
		}
	}

	fmt.Printf("Successfully loaded %d titles and %d pictures into database\n", len(titles), len(allPictures))
	return nil
}

func readPictureDirs() (map[string][]string, error) {
	dirPngs := make(map[string][]string)
	err := filepath.WalkDir(config.PicturesFolder, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), config.PicturesSuffix) {
			rel, _ := filepath.Rel(config.PicturesFolder, path)
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) == 2 {
				dirName := parts[0]
				dirPngs[dirName] = append(dirPngs[dirName], strings.TrimSuffix(parts[1], config.PicturesSuffix))
			}
		}
		return nil
	})
	return dirPngs, err
}

func setupRoutes() *gin.Engine {
	r := gin.Default()

	// Serve static files (frontend)
	r.Static("/static", "./static")
	r.LoadHTMLGlob("templates/*")

	// Serve OpenAPI spec from static file
	r.GET("/api/openapi.json", func(c *gin.Context) {
		c.File("docs/openapi.json")
	})

	// Frontend route
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{
			"title": "Xbox 360 Title Browser",
		})
	})

	api := r.Group("/api/v1")
	{
		api.GET("/search", searchTitles)
		api.GET("/titles", getTitles)
		api.GET("/titles/:id", getTitleByID)
		api.GET("/titles/:id/:picture", getTitlePicture)
	}

	return r
}

func getTitles(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	onlyWithPictures := c.DefaultQuery("only_with_pictures", "false") == "true"
	reverse := c.DefaultQuery("reverse", "false") == "true"

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}

	offset := (page - 1) * limit

	var titles []Title
	var total int64

	// Build query based on filter
	query := db.Model(&Title{})
	if onlyWithPictures {
		// Only get titles that have pictures
		query = query.Joins("JOIN pictures ON titles.title_id = pictures.title_id").Group("titles.title_id")
	}

	query.Count(&total)

	if reverse {
		query = query.Order("titles.title_id DESC")
	} else {
		query = query.Order("titles.title_id ASC")
	}

	// Get paginated titles with preloaded pictures
	result := query.Preload("Pictures").Offset(offset).Limit(limit).Find(&titles)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	pages := int((total + int64(limit) - 1) / int64(limit))

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:  titles,
		Total:  total,
		Limit:  limit,
		Offset: offset,
		Page:   page,
		Pages:  pages,
	})
}

func searchTitles(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query parameter 'q' is required"})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	onlyWithPictures := c.DefaultQuery("only_with_pictures", "false") == "true"

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}

	var allTitles []Title
	db.Preload("Pictures").Find(&allTitles)

	// Filter out titles with no pictures if onlyWithPictures is true
	if onlyWithPictures {
		filtered := make([]Title, 0, len(allTitles))
		for _, t := range allTitles {
			if len(t.Pictures) > 0 {
				filtered = append(filtered, t)
			}
		}
		allTitles = filtered
	}

	// Perform fuzzy search
	names := make([]string, len(allTitles))
	for i, title := range allTitles {
		names[i] = title.Name
	}

	matches := fuzzy.RankFindNormalizedFold(query, names)
	sort.Slice(matches, matches.Less)

	// Apply pagination to results
	total := len(matches)
	offset := (page - 1) * limit
	end := offset + limit
	if end > total {
		end = total
	}
	if offset > total {
		offset = total
	}

	var results []Title
	for i := offset; i < end; i++ {
		if i < len(matches) {
			results = append(results, allTitles[matches[i].OriginalIndex])
		}
	}

	pages := (total + limit - 1) / limit

	c.JSON(http.StatusOK, PaginatedResponse{
		Items:  results,
		Total:  int64(total),
		Limit:  limit,
		Offset: offset,
		Page:   page,
		Pages:  pages,
	})
}

func getTitleByID(c *gin.Context) {
	id := strings.ToLower(c.Param("id"))

	var title Title
	if err := db.Preload("Pictures").First(&title, "title_id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "Title not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
		return
	}

	c.JSON(http.StatusOK, title)
}

func getTitlePicture(c *gin.Context) {
	id := strings.ToLower(c.Param("id"))
	picture := strings.TrimSuffix(strings.ToLower(c.Param("picture")), config.PicturesSuffix)

	// Validate id and picture
	if len(id) != 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid title ID"})
		return
	}

	if len(picture) == 0 || len(picture) > 10 || strings.ContainsAny(picture, `/\`) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid picture name"})
		return
	}

	// Serve the actual file
	picturePath := filepath.Join(config.PicturesFolder, id, picture+config.PicturesSuffix)
	c.File(picturePath)
}

func main() {
	loadConfig()

	if err := initDB(); err != nil {
		fmt.Printf("Error initializing database: %v\n", err)
		os.Exit(1)
	}

	if err := loadTitlesToDB(); err != nil {
		fmt.Printf("Error loading data: %v\n", err)
		os.Exit(1)
	}

	r := setupRoutes()

	fmt.Printf("Server starting on %s\n", config.Address)
	fmt.Printf("Frontend available at: http://localhost%s\n", config.Address)
	fmt.Printf("API available at: http://localhost%s/api/v1\n", config.Address)

	if err := r.Run(config.Address); err != nil {
		fmt.Printf("Server failed to start: %v\n", err)
		os.Exit(1)
	}
}
