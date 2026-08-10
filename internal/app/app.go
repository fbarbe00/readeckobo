package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"golang.org/x/net/html"
	"readeckobo/internal/config"
	"readeckobo/internal/logger"
	"readeckobo/internal/models"
	"readeckobo/internal/readeck"
)

type App struct {
	Config             *config.Config
	Logger             *logger.Logger
	ImageHTTPClient    *http.Client
	ReadeckHTTPClient  *http.Client
	StoreAPIHTTPClient *http.Client
	FallbackHTTPClient *http.Client
}

func WithImageHTTPClient(client *http.Client) Option {
	return func(a *App) {
		a.ImageHTTPClient = client
	}
}

func WithStoreAPIHTTPClient(client *http.Client) Option {
	return func(a *App) {
		a.StoreAPIHTTPClient = client
	}
}

func WithFallbackHTTPClient(client *http.Client) Option {
	return func(a *App) {
		a.FallbackHTTPClient = client
	}
}

type Option func(*App)

func NewApp(opts ...Option) *App {
	app := &App{}
	for _, opt := range opts {
		opt(app)
	}
	return app
}

func WithConfig(cfg *config.Config) Option {
	return func(a *App) {
		a.Config = cfg
	}
}

func WithLogger(logger *logger.Logger) Option {
	return func(a *App) {
		a.Logger = logger
	}
}

func WithReadeckHTTPClient(client *http.Client) Option {
	return func(a *App) {
		a.ReadeckHTTPClient = client
	}
}

func (a *App) handleFullSync(ctx context.Context, readeckClient *readeck.Client, req *models.KoboGetRequest) (map[string]models.KoboArticleItem, int, error) {
	count, _ := strconv.Atoi(req.Count)
	offset, _ := strconv.Atoi(req.Offset)

	bsyncs, err := readeckClient.GetBookmarksSync(ctx, nil)
	if err != nil {
		a.Logger.Errorf("Full Sync: Error getting bookmark syncs: %v", err)
		return nil, 0, fmt.Errorf("failed to get bookmark syncs: %w", err)
	}
	a.Logger.Debugf("Full Sync: GetBookmarksSync returned %d sync events.", len(bsyncs))

	var candidateBookmarkIDs []string
	for _, bsync := range bsyncs {
		if bsync.Type != "delete" {
			candidateBookmarkIDs = append(candidateBookmarkIDs, bsync.ID)
		}
	}

	bookmarksDetailsMap, err := readeckClient.SyncBookmarksContent(ctx, candidateBookmarkIDs)
	if err != nil {
		a.Logger.Errorf("Full Sync: Error getting bookmark details: %v", err)
		return nil, 0, fmt.Errorf("failed to get bookmark details: %w", err)
	}

	actualBookmarks := []models.KoboArticleItem{}
	for _, bsync := range bsyncs {
		if bsync.Type == "delete" {
			continue
		}
		bookmark, found := bookmarksDetailsMap[bsync.ID]
		if !found || bookmark == nil || bookmark.IsArchived {
			continue
		}

		favoriteStatus := "0"
		if bookmark.IsMarked {
			favoriteStatus = "1"
		}

		entry := buildKoboArticleItem(bookmark, &bsync)
		entry.Status = "0"
		entry.Favorite = favoriteStatus
		actualBookmarks = append(actualBookmarks, entry)
	}

	totalNonArchivedBookmarks := len(actualBookmarks)
	resultList := make(map[string]models.KoboArticleItem)

	startIndex := offset
	endIndex := offset + count
	if count == 0 {
		endIndex = len(actualBookmarks)
	}
	if startIndex > len(actualBookmarks) {
		startIndex = len(actualBookmarks)
	}
	if endIndex > len(actualBookmarks) {
		endIndex = len(actualBookmarks)
	}

	for _, bm := range actualBookmarks[startIndex:endIndex] {
		resultList[bm.ItemID] = bm
	}

	return resultList, totalNonArchivedBookmarks, nil
}

func (a *App) handleIncrementalSync(ctx context.Context, readeckClient *readeck.Client, since *time.Time) (map[string]models.KoboArticleItem, int, error) {
	resultList := make(map[string]models.KoboArticleItem)

	bsyncs, err := readeckClient.GetBookmarksSync(ctx, since)
	if err != nil {
		a.Logger.Errorf("Incremental Sync: Error getting bookmark syncs: %v", err)
		return nil, 0, fmt.Errorf("failed to get bookmark syncs: %w", err)
	}
	a.Logger.Debugf("Incremental Sync: GetBookmarksSync returned %d sync events.", len(bsyncs))

	var candidateBookmarkIDs []string
	for _, bsync := range bsyncs {
		if bsync.Type == "delete" {
			resultList[bsync.ID] = models.KoboArticleItem{ItemID: bsync.ID, Status: "2"}
		} else {
			candidateBookmarkIDs = append(candidateBookmarkIDs, bsync.ID)
		}
	}

	if len(candidateBookmarkIDs) == 0 {
		return resultList, 0, nil
	}

	bookmarksDetailsMap, err := readeckClient.SyncBookmarksContent(ctx, candidateBookmarkIDs)
	if err != nil {
		a.Logger.Errorf("Incremental Sync: Error getting bookmark details: %v", err)
		return nil, 0, fmt.Errorf("failed to get bookmark details: %w", err)
	}

	totalNonArchivedBookmarks := 0
	for _, bsync := range bsyncs {
		if bsync.Type == "delete" {
			continue
		}

		bookmark, found := bookmarksDetailsMap[bsync.ID]
		if !found || bookmark == nil {
			continue
		}

		favoriteStatus := "0"
		if bookmark.IsMarked {
			favoriteStatus = "1"
		}

		entry := buildKoboArticleItem(bookmark, &bsync)
		entry.Favorite = favoriteStatus

		if bookmark.IsArchived {
			entry.Status = "1"
		} else {
			entry.Status = "0"
			totalNonArchivedBookmarks++
		}
		resultList[bookmark.ID] = entry
	}

	return resultList, totalNonArchivedBookmarks, nil
}

func (a *App) HandleKoboGet(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		a.Logger.Errorf("Error reading /api/kobo/get request body: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	a.Logger.Debugf("Incoming Kobo Request for /api/kobo/get:\nMethod: %s\nURL: %s\nHeaders: %v\nBody: %s", r.Method, r.URL, r.Header, string(bodyBytes))

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.KoboGetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		a.Logger.Errorf("Error decoding /api/kobo/get request: %v, body: %s, URL: %s, Params: %v", err, string(bodyBytes), r.URL.Path, r.URL.Query())
		return
	}

	user, err := a.getUser(req.AccessToken)
	if err != nil {
		http.Error(w, "Invalid access token", http.StatusUnauthorized)
		a.Logger.Errorf("Error authenticating token for /api/kobo/get: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	readeckClient, err := a.newReadeckClient(user)
	if err != nil {
		http.Error(w, "Failed to initialize Readeck client", http.StatusInternalServerError)
		a.Logger.Errorf("Error initializing Readeck client for /api/kobo/get: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	var since *time.Time
	if req.Since != nil {
		a.Logger.Debugf("Received 'since' parameter with value: %v (type: %T)", req.Since, req.Since)
		if v, ok := req.Since.(float64); ok {
			t := time.Unix(int64(v), 0)
			since = &t
		} else {
			a.Logger.Warnf("Unexpected type for 'since' parameter: %T. Expected float64 or nil.", req.Since)
		}
	}

	var resultList map[string]models.KoboArticleItem
	var total int

	if since == nil {
		a.Logger.Debugf("Handling full sync.")
		resultList, total, err = a.handleFullSync(r.Context(), readeckClient, &req)
	} else {
		a.Logger.Debugf("Handling incremental sync.")
		resultList, total, err = a.handleIncrementalSync(r.Context(), readeckClient, since)
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := models.KoboGetResponse{
		Status: 1,
		List:   resultList,
		Total:  total,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		a.Logger.Errorf("Error encoding response for /api/kobo/get: %v", err)
	}
}

func buildKoboArticleItem(bookmark *readeck.Bookmark, bsync *readeck.BookmarkSync) models.KoboArticleItem {
	authors := make(map[string]models.KoboAuthor)
	for _, author := range bookmark.Authors {
		authors[author] = models.KoboAuthor{AuthorID: author, Name: author}
	}

	tags := make(map[string]models.KoboTag)
	for _, label := range bookmark.Labels {
		tags[label] = models.KoboTag{ItemID: bsync.ID, Tag: label}
	}

	entry := models.KoboArticleItem{
		Authors:       authors,
		Excerpt:       bookmark.Description,
		GivenTitle:    bookmark.Title,
		GivenURL:      bookmark.URL,
		HasImage:      "0",
		HasVideo:      "0",
		Image:         &models.KoboImage{},
		Images:        make(map[string]models.KoboImage),
		IsArticle:     "1",
		ItemID:        bookmark.ID,
		ResolvedID:    bookmark.ID,
		ResolvedTitle: bookmark.Title,
		ResolvedURL:   bookmark.URL,
		Tags:          tags,
		TimeAdded:     bookmark.Created.Unix(),
		TimeRead:      0,
		TimeUpdated:   bookmark.Updated.Unix(),
		Videos:        []any{},
		WordCount:     bookmark.WordCount,
		Optional:      make(map[string]any),
	}

	if bookmark.Resources.Image != nil && bookmark.Resources.Image.Src != "" {
		entry.HasImage = "1"
		entry.Image = &models.KoboImage{Src: bookmark.Resources.Image.Src}
		entry.Images["1"] = models.KoboImage{
			ImageID: "1",
			ItemID:  "1",
			Src:     bookmark.Resources.Image.Src,
		}
		entry.Optional["top_image_url"] = bookmark.Resources.Image.Src
	}

	return entry
}

func (a *App) HandleKoboDownload(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		a.Logger.Errorf("Error reading /api/kobo/download request body: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	a.Logger.Debugf("Incoming Kobo Request for /api/kobo/download:\nMethod: %s\nURL: %s\nHeaders: %v\nBody: %s", r.Method, r.URL, r.Header, string(bodyBytes))

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.KoboDownloadRequest
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&req); err != nil {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid request body or form data", http.StatusBadRequest)
			a.Logger.Errorf("Error decoding /api/kobo/download request: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
			return
		}
		req.AccessToken = r.FormValue("access_token")
		req.ConsumerKey = r.FormValue("consumer_key")
		req.Images, _ = strconv.Atoi(r.FormValue("images"))
		req.Refresh, _ = strconv.Atoi(r.FormValue("refresh"))
		req.Output = r.FormValue("output")
		req.URL = r.FormValue("url")
	}

	user, err := a.getUser(req.AccessToken)
	if err != nil {
		http.Error(w, "Invalid access token", http.StatusUnauthorized)
		a.Logger.Errorf("Error authenticating token for /api/kobo/download: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	readeckClient, err := a.newReadeckClient(user)
	if err != nil {
		http.Error(w, "Failed to initialize Readeck client", http.StatusInternalServerError)
		a.Logger.Errorf("Error initializing Readeck client for /api/kobo/download: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	reqURLStr := req.URL
	if reqURLStr == "" {
		http.Error(w, "Missing 'url' parameter", http.StatusBadRequest)
		a.Logger.Errorf("Error: Missing 'url' parameter in /api/kobo/download request, URL: %s, Params: %v", r.URL.Path, r.URL.Query())
		return
	}

	parsedURL, err := url.Parse(reqURLStr)
	if err != nil {
		http.Error(w, "Invalid 'url' parameter", http.StatusBadRequest)
		a.Logger.Errorf("Error: Invalid 'url' parameter in /api/kobo/download request: %v, url: %s, URL: %s, Params: %v", err, reqURLStr, r.URL.Path, r.URL.Query())
		return
	}

	var bookmarkFound *readeck.Bookmark
	sitesToTry := getSitesToTry(parsedURL.Host)
	ctx := r.Context()

	for _, site := range sitesToTry {
		currentPage := 1
		totalPages := 1

		for currentPage <= totalPages {
			isArchived := false
			bookmarks, tp, err := readeckClient.GetBookmarks(ctx, site, currentPage, &isArchived)
			if err != nil {
				a.Logger.Warnf("Error searching Readeck bookmarks for site %s, page %d in /api/kobo/download: %v, URL: %s, Params: %v", site, currentPage, err, r.URL.Path, r.URL.Query())
				break
			}
			totalPages = tp

			for i := range bookmarks {
				if bookmarks[i].URL != "" {
					match, err := compareURLs(bookmarks[i].URL, reqURLStr)
					if err != nil {
						a.Logger.Warnf("Error comparing URLs for bookmark %s in /api/kobo/download: %v, URL: %s, Params: %v", bookmarks[i].ID, err, r.URL.Path, r.URL.Query())
						continue
					}
					if match {
						bookmarkFound = &bookmarks[i]
						break
					}
				}
			}
			if bookmarkFound != nil {
				break
			}
			currentPage++
		}
		if bookmarkFound != nil {
			break
		}
	}

	if bookmarkFound == nil {
		http.Error(w, "Article not found", http.StatusNotFound)
		return
	}

	articleHTML, err := readeckClient.GetBookmarkArticle(ctx, bookmarkFound.ID)
	if err != nil {
		http.Error(w, "Failed to fetch article content", http.StatusInternalServerError)
		a.Logger.Errorf("Error fetching article content for bookmark %s in /api/kobo/download: %v, URL: %s, Params: %v", bookmarkFound.ID, err, r.URL.Path, r.URL.Query())
		return
	}

	doc, err := html.Parse(strings.NewReader(articleHTML))
	if err != nil {
		http.Error(w, "Failed to parse article HTML", http.StatusInternalServerError)
		a.Logger.Errorf("Error parsing article HTML for bookmark %s in /api/kobo/download: %v, URL: %s, Params: %v", bookmarkFound.ID, err, r.URL.Path, r.URL.Query())
		return
	}

	images := make(map[string]any)
	var imageIndex int
	var processNode func(*html.Node)
	processNode = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, attr := range n.Attr {
				if attr.Key == "src" {
					src := attr.Val
					images[fmt.Sprintf("%d", imageIndex)] = map[string]any{
						"image_id": fmt.Sprintf("%d", imageIndex),
						"item_id":  fmt.Sprintf("%d", imageIndex),
						"src":      src,
					}
					comment := &html.Node{
						Type: html.CommentNode,
						Data: fmt.Sprintf("IMG_%d", imageIndex),
					}
					if n.Parent != nil {
						n.Parent.InsertBefore(comment, n)
						n.Parent.RemoveChild(n)
					}
					imageIndex++
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			processNode(c)
		}
	}
	processNode(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		http.Error(w, "Failed to render modified HTML", http.StatusInternalServerError)
		a.Logger.Errorf("Error rendering modified HTML for bookmark %s in /api/kobo/download: %v, URL: %s, Params: %v", bookmarkFound.ID, err, r.URL.Path, r.URL.Query())
		return
	}

	response := map[string]any{
		"images":  images,
		"article": buf.String(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.Logger.Errorf("Error encoding response for /api/kobo/download: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
	}
}

func getSitesToTry(host string) []string {
	var sites []string
	parts := strings.Split(host, ".")

	sites = append(sites, host)

	if len(parts) >= 2 {
		siteName := parts[len(parts)-2]
		if siteName != "" && siteName != host {
			sites = append(sites, siteName)
		}
	}

	uniqueSites := make([]string, 0, len(sites))
	seen := make(map[string]bool)
	for _, site := range sites {
		if _, ok := seen[site]; !ok {
			seen[site] = true
			uniqueSites = append(uniqueSites, site)
		}
	}

	return uniqueSites
}

func (a *App) HandleKoboSend(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		a.Logger.Errorf("Error reading /api/kobo/send request body: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	a.Logger.Debugf("Incoming Kobo Request for /api/kobo/send:\nMethod: %s\nURL: %s\nHeaders: %v\nBody: %s", r.Method, r.URL, r.Header, string(bodyBytes))

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.KoboSendRequest
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		a.Logger.Errorf("Error decoding /api/kobo/send request: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	user, err := a.getUser(req.AccessToken)
	if err != nil {
		http.Error(w, "Invalid access token", http.StatusUnauthorized)
		a.Logger.Errorf("Error authenticating token for /api/kobo/send: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	readeckClient, err := a.newReadeckClient(user)
	if err != nil {
		http.Error(w, "Failed to initialize Readeck client", http.StatusInternalServerError)
		a.Logger.Errorf("Error initializing Readeck client for /api/kobo/send: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
		return
	}

	ctx := r.Context()
	actionResults := make([]bool, len(req.Actions))
	allSucceeded := true

	for i, actionInterface := range req.Actions {
		actionMap, ok := actionInterface.(map[string]any)
		if !ok {
			actionResults[i] = false
			allSucceeded = false
			continue
		}

		action, _ := actionMap["action"].(string)
		var err error

		switch action {
		case "archive":
			itemID, _ := actionMap["item_id"].(string)
			err = readeckClient.UpdateBookmark(ctx, itemID, map[string]any{"is_archived": true})
		case "readd":
			itemID, _ := actionMap["item_id"].(string)
			err = readeckClient.UpdateBookmark(ctx, itemID, map[string]any{"is_archived": false})
		case "favorite":
			itemID, _ := actionMap["item_id"].(string)
			err = readeckClient.UpdateBookmark(ctx, itemID, map[string]any{"is_marked": true})
		case "unfavorite":
			itemID, _ := actionMap["item_id"].(string)
			err = readeckClient.UpdateBookmark(ctx, itemID, map[string]any{"is_marked": false})
		case "delete":
			itemID, _ := actionMap["item_id"].(string)
			err = readeckClient.UpdateBookmark(ctx, itemID, map[string]any{"is_deleted": true})
		case "add":
			url, _ := actionMap["url"].(string)
			err = readeckClient.CreateBookmark(ctx, url)
		case "opened_item", "left_item":
			err = nil
		default:
			err = fmt.Errorf("unknown action: %s", action)
		}

		if err != nil {
			a.Logger.Warnf("Error processing action '%s' in /api/kobo/send: %v, URL: %s, Params: %v", action, err, r.URL.Path, r.URL.Query())
			actionResults[i] = false
			allSucceeded = false
		} else {
			actionResults[i] = true
		}
	}

	response := map[string]any{
		"status":         allSucceeded,
		"action_results": actionResults,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		a.Logger.Errorf("Error encoding response for /api/kobo/send: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
	}
}

func (a *App) HandleConvertImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	imageURL := r.URL.Query().Get("url")
	if imageURL == "" {
		http.Error(w, "Missing 'url' parameter", http.StatusBadRequest)
		return
	}

	client := a.ImageHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Get(imageURL)
	if err != nil {
		a.Logger.Errorf("Failed to fetch image %s in /api/convert-image: %v, URL: %s, Params: %v", imageURL, err, r.URL.Path, r.URL.Query())
		a.returnPlaceholderImage(w, r, "Image fetch failed")
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			a.Logger.Warnf("Error closing response body for image %s in /api/convert-image: %v, URL: %s, Params: %v", imageURL, err, r.URL.Path, r.URL.Query())
		}
	}()

	if resp.StatusCode != http.StatusOK {
		a.Logger.Warnf("Failed to fetch image %s in /api/convert-image: status %d, URL: %s, Params: %v", imageURL, resp.StatusCode, r.URL.Path, r.URL.Query())
		a.returnPlaceholderImage(w, r, "Image not found")
		return
	}

	img, _, err := image.Decode(resp.Body)
	if err != nil {
		a.Logger.Warnf("Failed to decode image %s in /api/convert-image: %v, URL: %s, Params: %v", imageURL, err, r.URL.Path, r.URL.Query())
		a.returnPlaceholderImage(w, r, "Image decoding failed")
		return
	}

	b := img.Bounds()
	rgbImg := image.NewRGBA(b)
	draw.Draw(rgbImg, b, img, image.Point{}, draw.Src)

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if err := jpeg.Encode(w, rgbImg, &jpeg.Options{Quality: 85}); err != nil {
		a.Logger.Errorf("Failed to encode JPEG for image %s in /api/convert-image: %v, URL: %s, Params: %v", imageURL, err, r.URL.Path, r.URL.Query())
	}
}

func (a *App) returnPlaceholderImage(w http.ResponseWriter, r *http.Request, message string) {
	width, height := 800, 600
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)

	col := image.Black
	point := fixed.Point26_6{X: fixed.Int26_6(20 * 64), Y: fixed.Int26_6(300 * 64)}
	d := &font.Drawer{
		Dst:  img,
		Src:  col,
		Face: basicfont.Face7x13,
		Dot:  point,
	}
	d.DrawString(message)

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-control", "public, max-age=300")
	if err := jpeg.Encode(w, img, &jpeg.Options{Quality: 85}); err != nil {
		a.Logger.Errorf("Error encoding placeholder image: %v, URL: %s, Params: %v", err, r.URL.Path, r.URL.Query())
	}
}

func compareURLs(url1, url2 string) (bool, error) {
	u1, err := url.Parse(strings.TrimSpace(url1))
	if err != nil {
		return false, err
	}
	u2, err := url.Parse(strings.TrimSpace(url2))
	if err != nil {
		return false, err
	}

	u1.Host = strings.TrimPrefix(u1.Host, "www.")
	u2.Host = strings.TrimPrefix(u2.Host, "www.")

	return u1.Scheme == u2.Scheme && u1.Host == u2.Host && u1.Path == u2.Path, nil
}

func (a *App) getUser(deviceToken string) (*config.User, error) {
	for i := range a.Config.Users {
		if a.Config.Users[i].Token == deviceToken {
			return &a.Config.Users[i], nil
		}
	}
	return nil, fmt.Errorf("unauthorized device token")
}

func (a *App) newReadeckClient(user *config.User) (*readeck.Client, error) {
	host := user.ReadeckHost
	if host == "" {
		host = a.Config.Readeck.Host
	}
	return readeck.NewClient(host, user.ReadeckAccessToken, a.Logger, a.ReadeckHTTPClient)
}

func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		parts := strings.Split(proto, ",")
		return strings.TrimSpace(parts[0])
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (a *App) storeAPIHTTPClient() *http.Client {
	if a.StoreAPIHTTPClient != nil {
		return a.StoreAPIHTTPClient
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
}

func (a *App) HandleStoreAPIProxy(w http.ResponseWriter, r *http.Request) {
	targetHost := a.Config.Kobo.StoreAPIHost
	if targetHost == "" {
		targetHost = "storeapi.kobo.com"
	}

	proxy := &httputil.ReverseProxy{
		Transport: a.storeAPIHTTPClient().Transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = targetHost
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/instapaper-proxy/storeapi")
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
			req.Host = targetHost
		},
	}
	proxy.ServeHTTP(w, r)
}

func (a *App) HandleStoreAPIInitialization(w http.ResponseWriter, r *http.Request) {
	targetHost := a.Config.Kobo.StoreAPIHost
	if targetHost == "" {
		targetHost = "storeapi.kobo.com"
	}

	rp := &httputil.ReverseProxy{
		Transport: a.storeAPIHTTPClient().Transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = targetHost
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/instapaper-proxy/storeapi")
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
			req.Host = targetHost
			req.Header.Set("Accept-Encoding", "")
		},
		ModifyResponse: a.rewriteInitializationResponse(r),
	}
	rp.ServeHTTP(w, r)
}

func (a *App) rewriteInitializationResponse(r *http.Request) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			return nil
		}

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if err != nil {
			return err
		}
		_ = resp.Body.Close()

		prefix := requestScheme(r) + "://" + r.Host + "/instapaper-proxy/instapaper"
		body := strings.ReplaceAll(string(bodyBytes), "https://www.instapaper.com", prefix)
		body = strings.ReplaceAll(body, "http://www.instapaper.com", prefix)

		resp.Body = io.NopCloser(strings.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
}

func (a *App) HandleFallbackProxy(w http.ResponseWriter, r *http.Request) {
	if a.Config.Kobo.FallbackURL == "" {
		http.NotFound(w, r)
		return
	}

	target, err := url.Parse(a.Config.Kobo.FallbackURL)
	if err != nil {
		http.Error(w, "Invalid fallback URL configuration", http.StatusInternalServerError)
		return
	}

	transport := http.DefaultTransport
	if a.FallbackHTTPClient != nil && a.FallbackHTTPClient.Transport != nil {
		transport = a.FallbackHTTPClient.Transport
	}

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = joinURLPath(target.Path, req.URL.Path)
			req.Host = target.Host
			req.Header.Set("X-Forwarded-Host", r.Host)
			req.Header.Set("X-Forwarded-Proto", requestScheme(r))
			req.Header.Set("X-Scheme", requestScheme(r))
			if strings.HasSuffix(r.URL.Path, "/v1/initialization") {
				req.Header.Set("Accept-Encoding", "")
			}
		},
	}
	if strings.HasSuffix(r.URL.Path, "/v1/initialization") {
		proxy.ModifyResponse = a.rewriteInitializationResponse(r)
	}
	proxy.ServeHTTP(w, r)
}

func joinURLPath(basePath, requestPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		return "/" + strings.TrimLeft(requestPath, "/")
	}
	return basePath + "/" + strings.TrimLeft(requestPath, "/")
}

func (a *App) HandleInstapaperProxy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	r.URL.Path = "/" + strings.TrimLeft(strings.TrimPrefix(r.URL.Path, "/instapaper-proxy/instapaper"), "/")

	var handler http.HandlerFunc
	switch r.URL.Path {
	case "/api/kobo/get":
		handler = a.HandleKoboGet
	case "/api/kobo/download":
		handler = a.HandleKoboDownload
	case "/api/kobo/send":
		handler = a.HandleKoboSend
	case "/api/convert-image":
		handler = a.HandleConvertImage
	default:
		http.NotFound(w, r)
		return
	}
	handler.ServeHTTP(w, r)
}
