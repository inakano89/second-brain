package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/queue"
)

const (
	driveAPI    = "https://www.googleapis.com/drive/v3/files"
	driveFields = "id,name,mimeType,modifiedTime,createdTime,webViewLink,size,owners(displayName,emailAddress)"

	mimeGDoc   = "application/vnd.google-apps.document"
	mimeGSheet = "application/vnd.google-apps.spreadsheet"
	mimeGSlide = "application/vnd.google-apps.presentation"
	mimeDocx   = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

	maxDriveDownload = 25 << 20
	maxDriveExport   = 10 << 20
	maxDriveText     = 100000
)

// driveKinds maps importable MIME types to a tag.
var driveKinds = map[string]string{
	mimeGDoc: "documento", mimeGSheet: "planilha", mimeGSlide: "apresentacao", mimeDocx: "documento",
	"application/pdf": "pdf", "text/plain": "texto", "text/markdown": "texto", "text/csv": "planilha",
}

type gDriveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	WebViewLink  string `json:"webViewLink"`
	Size         string `json:"size"`
	ModifiedTime string `json:"modifiedTime"`
	CreatedTime  string `json:"createdTime"`
	Owners       []struct {
		DisplayName  string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
	} `json:"owners"`
}

func (f gDriveFile) public() agent.DriveFile {
	d := agent.DriveFile{ID: f.ID, Name: f.Name, MIME: f.MimeType, Link: f.WebViewLink}
	d.Size, _ = strconv.ParseInt(f.Size, 10, 64)
	d.Created, _ = time.Parse(time.RFC3339, f.CreatedTime)
	d.Modified, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	if len(f.Owners) > 0 {
		d.Owner = f.Owners[0].DisplayName
		if d.Owner == "" {
			d.Owner = f.Owners[0].EmailAddress
		}
	}
	return d
}

// driveQuote quotes a string literal for the Drive query language.
func driveQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func mimeClause() string {
	mimes := make([]string, 0, len(driveKinds))
	for m := range driveKinds {
		mimes = append(mimes, "mimeType = "+driveQuote(m))
	}
	sort.Strings(mimes)
	return "(" + strings.Join(mimes, " or ") + ")"
}

func (c *Client) listDrive(ctx context.Context, q, orderBy string, pageSize int, pageToken string) ([]gDriveFile, string, error) {
	v := url.Values{"q": {q}, "fields": {"nextPageToken,files(" + driveFields + ")"}, "pageSize": {strconv.Itoa(pageSize)}, "spaces": {"drive"}}
	if orderBy != "" {
		v.Set("orderBy", orderBy)
	}
	if pageToken != "" {
		v.Set("pageToken", pageToken)
	}
	var resp struct {
		Files         []gDriveFile `json:"files"`
		NextPageToken string       `json:"nextPageToken"`
	}
	if err := c.do(ctx, http.MethodGet, driveAPI+"?"+v.Encode(), nil, &resp); err != nil {
		return nil, "", err
	}
	return resp.Files, resp.NextPageToken, nil
}

// SearchDrive implements agent.GoogleAPI.
func (c *Client) SearchDrive(ctx context.Context, query string, max int) ([]agent.DriveFile, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("consulta vazia")
	}
	q := "trashed = false and (name contains " + driveQuote(query) + " or fullText contains " + driveQuote(query) + ")"
	files, _, err := c.listDrive(ctx, q, "", max, "")
	if err != nil {
		return nil, err
	}
	out := make([]agent.DriveFile, len(files))
	for i, f := range files {
		out[i] = f.public()
	}
	return out, nil
}

// ReadDriveFile implements agent.GoogleAPI.
func (c *Client) ReadDriveFile(ctx context.Context, id string) (*agent.DriveFile, string, error) {
	var f gDriveFile
	if err := c.do(ctx, http.MethodGet, driveAPI+"/"+url.PathEscape(id)+"?fields="+url.QueryEscape(driveFields)+"&supportsAllDrives=true", nil, &f); err != nil {
		return nil, "", err
	}
	text, err := c.fileText(ctx, f)
	pub := f.public()
	return &pub, text, err
}

func (c *Client) export(ctx context.Context, id, mimeType string) (string, error) {
	b, err := c.readAll(ctx, driveAPI+"/"+url.PathEscape(id)+"/export?mimeType="+url.QueryEscape(mimeType), maxDriveExport)
	return string(b), err
}

func (c *Client) download(ctx context.Context, id string) ([]byte, error) {
	return c.readAll(ctx, driveAPI+"/"+url.PathEscape(id)+"?alt=media&supportsAllDrives=true", maxDriveDownload)
}

// fileText extracts the text of a supported Drive file.
func (c *Client) fileText(ctx context.Context, f gDriveFile) (string, error) {
	if n, _ := strconv.ParseInt(f.Size, 10, 64); n > maxDriveDownload {
		return "", queue.Permanentf("%s: arquivo grande demais (%d MB)", f.Name, n>>20)
	}
	switch f.MimeType {
	case mimeGDoc:
		text, err := c.export(ctx, f.ID, "text/markdown")
		if isStatus(err, http.StatusBadRequest) {
			text, err = c.export(ctx, f.ID, "text/plain")
		}
		return text, err
	case mimeGSheet:
		return c.export(ctx, f.ID, "text/csv")
	case mimeGSlide:
		return c.export(ctx, f.ID, "text/plain")
	case "application/pdf":
		data, err := c.download(ctx, f.ID)
		if err != nil {
			return "", err
		}
		text, err := extract.PDFBytes(data)
		if err != nil {
			return "", queue.Permanent(err)
		}
		return text, nil
	case mimeDocx:
		data, err := c.download(ctx, f.ID)
		if err != nil {
			return "", err
		}
		text, err := extract.DOCXBytes(data)
		if err != nil {
			return "", queue.Permanent(err)
		}
		return text, nil
	}
	if strings.HasPrefix(f.MimeType, "text/") {
		data, err := c.download(ctx, f.ID)
		return string(data), err
	}
	return "", queue.Permanentf("tipo de arquivo do Drive não suportado: %s", f.MimeType)
}

// TakeoutArchives lists Google Takeout .zip exports saved to Drive after since (oldest first).
func (c *Client) TakeoutArchives(ctx context.Context, since time.Time) ([]agent.DriveFile, error) {
	q := "trashed = false and name contains 'takeout-' and (mimeType = 'application/zip' or mimeType = 'application/x-zip-compressed' or mimeType = 'application/x-zip')"
	if !since.IsZero() {
		q += " and createdTime > " + driveQuote(since.UTC().Format(time.RFC3339Nano))
	}
	var out []agent.DriveFile
	token := ""
	for page := 0; page < 10; page++ {
		files, next, err := c.listDrive(ctx, q, "createdTime", 100, token)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, f.public())
		}
		if next == "" {
			break
		}
		token = next
	}
	return out, nil
}

// Download streams a Drive file into w (no size limit: used for Takeout archives).
func (c *Client) Download(ctx context.Context, id string, w io.Writer) error {
	resp, err := c.open(ctx, http.MethodGet, driveAPI+"/"+url.PathEscape(id)+"?alt=media", nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

const driveCursorKey = "google.drive.cursor"

// SyncDrive imports documents modified since the last run (oldest first, DRIVE_MAX_FILES per run).
func (s *Syncer) SyncDrive(ctx context.Context) (int, error) {
	if !s.g.Can(agent.GoogleDrive) {
		return 0, nil
	}
	cfg, db := s.g.cfg, s.ag.DB()
	cursor, ok, err := db.KVGet(ctx, driveCursorKey)
	if err != nil {
		return 0, err
	}
	if !ok {
		cursor = "1970-01-01T00:00:00Z"
		if days := cfg.GetInt("DRIVE_SINCE_DAYS", 365); days > 0 {
			cursor = time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339)
		}
	}
	q := "trashed = false and modifiedTime > " + driveQuote(cursor) + " and " + mimeClause()
	if extra := strings.TrimSpace(cfg.Get("DRIVE_QUERY")); extra != "" {
		q += " and (" + extra + ")"
	}
	max := min(max(cfg.GetInt("DRIVE_MAX_FILES", 40), 1), 200)
	files, _, err := s.g.listDrive(ctx, q, "modifiedTime", max, "")
	if err != nil {
		return 0, err
	}
	okFiles := make([]bool, len(files))
	var mu sync.Mutex
	count := 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, f := range files {
		g.Go(func() error {
			changed, err := s.ingestDriveFile(gctx, f)
			if err != nil {
				if !queue.IsPermanent(err) {
					return err
				}
				s.g.log.Warn("arquivo do Drive ignorado", "file", f.Name, "err", err)
			}
			okFiles[i] = true
			if changed {
				mu.Lock()
				count++
				mu.Unlock()
			}
			return nil
		})
	}
	werr := g.Wait()
	last := ""
	for i := range files { // advance through the longest fully processed prefix
		if !okFiles[i] {
			break
		}
		last = files[i].ModifiedTime
	}
	if last != "" {
		if err := db.KVSet(ctx, driveCursorKey, last); err != nil {
			return count, err
		}
	}
	if werr == nil && len(files) == max && last != "" { // backlog left: continue shortly
		_, _ = db.Enqueue(ctx, TaskDriveSync, nil, database.EnqueueOpts{DedupeKey: TaskDriveSync + ":" + last, Delay: 2 * time.Minute, MaxAttempts: 3})
	}
	return count, werr
}

func (s *Syncer) ingestDriveFile(ctx context.Context, f gDriveFile) (bool, error) {
	text, err := s.g.fileText(ctx, f)
	if err != nil {
		return false, err
	}
	text = strings.TrimSpace(text)
	hash := hashOf(f.Name, text)
	if s.unchanged(ctx, "drive", f.ID, hash) {
		return false, nil
	}
	pub := f.public()
	loc := s.g.cfg.Location()
	var b strings.Builder
	fmt.Fprintf(&b, "**Arquivo:** [%s](%s)\n**Modificado:** %s\n", f.Name, f.WebViewLink, pub.Modified.In(loc).Format("02/01/2006 15:04"))
	if pub.Owner != "" {
		fmt.Fprintf(&b, "**Dono:** %s\n", pub.Owner)
	}
	if text == "" {
		b.WriteString("\n_(sem texto extraível)_\n")
	} else {
		b.WriteString("\n" + extract.Truncate(text, maxDriveText))
	}
	_, _, err = s.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypeNote, Title: f.Name, Content: b.String(), Tags: []string{"drive", driveKinds[f.MimeType]},
		Source: "drive", SourceRef: f.ID, CreatedAt: pub.Created, Enrich: true,
		Meta: map[string]any{"link": f.WebViewLink, "mime": f.MimeType, "modified": f.ModifiedTime, "owner": pub.Owner, "hash": hash, "enriched": false},
	})
	return err == nil, err
}
