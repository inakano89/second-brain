package scheduler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
)

// BackupDir is where encrypted snapshots are kept locally.
func BackupDir(cfg *config.Config) string { return filepath.Join(cfg.GetPath("DATA_DIR"), "backups") }

// Backup snapshots SQLite (VACUUM INTO), encrypts it with AES-256-GCM and ships it
// to every configured target concurrently.
func (d *Deps) Backup(ctx context.Context) (string, error) {
	key := d.Cfg.Get("BACKUP_ENCRYPTION_KEY")
	if key == "" {
		return "", errors.New("backup: BACKUP_ENCRYPTION_KEY não configurada")
	}
	dir := BackupDir(d.Cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	snap := filepath.Join(dir, ".snapshot-"+stamp+".db")
	defer os.Remove(snap)
	if err := d.DB.Snapshot(ctx, snap); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	name := "second-brain-" + stamp + ".db.enc"
	enc := filepath.Join(dir, name)
	if err := crypto.EncryptFile(snap, enc, key); err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
		sent []string
	)
	for _, target := range d.Cfg.GetList("BACKUP_TARGETS") {
		target = strings.ToLower(target)
		var fn func(context.Context) error
		switch target {
		case "local":
			continue
		case "s3":
			fn = func(ctx context.Context) error {
				return uploadS3(ctx, d.Cfg, enc, strings.TrimLeft(d.Cfg.Get("S3_PREFIX"), "/")+name)
			}
		case "webdav":
			fn = func(ctx context.Context) error { return uploadWebDAV(ctx, d.Cfg, enc, name) }
		case "telegram":
			fn = func(ctx context.Context) error { return d.uploadTelegram(ctx, enc, name) }
		default:
			errs = append(errs, fmt.Errorf("destino de backup desconhecido: %s", target))
			continue
		}
		wg.Add(1)
		go func(target string, fn func(context.Context) error) {
			defer wg.Done()
			err := fn(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", target, err))
			} else {
				sent = append(sent, target)
			}
		}(target, fn)
	}
	wg.Wait()
	rotate(dir, d.Cfg.GetInt("BACKUP_KEEP", 7))
	st, _ := os.Stat(enc)
	var size int64
	if st != nil {
		size = st.Size()
	}
	d.Log.Info("backup concluído", "file", name, "bytes", size, "targets", sent, "errors", len(errs))
	return enc, errors.Join(errs...)
}

func rotate(dir string, keep int) {
	if keep <= 0 {
		keep = 7
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "second-brain-*.db.enc"))
	sort.Strings(matches)
	for i := 0; i < len(matches)-keep; i++ {
		os.Remove(matches[i])
	}
}

func (d *Deps) uploadTelegram(ctx context.Context, path, name string) error {
	if d.Notifier == nil {
		return errors.New("telegram não configurado")
	}
	var chat int64
	if s := d.Cfg.Get("BACKUP_TELEGRAM_CHAT_ID"); s != "" {
		chat, _ = strconv.ParseInt(s, 10, 64)
	} else if ids := d.Notifier.AllowedIDs(); len(ids) > 0 {
		chat = ids[0]
	}
	if chat == 0 {
		return errors.New("nenhum chat de backup definido")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() > 49<<20 {
		return fmt.Errorf("arquivo de %d MB excede o limite de 50 MB da Bot API", st.Size()>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return d.Notifier.SendDocument(ctx, chat, name, f, "🔐 Backup cifrado (AES-256-GCM) — "+time.Now().Format("02/01/2006 15:04"))
}

func uploadWebDAV(ctx context.Context, cfg *config.Config, path, name string) error {
	base := strings.TrimRight(cfg.Get("WEBDAV_URL"), "/")
	if base == "" {
		return errors.New("WEBDAV_URL vazio")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/"+url.PathEscape(name), f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	if u := cfg.Get("WEBDAV_USER"); u != "" {
		req.SetBasicAuth(u, cfg.Get("WEBDAV_PASSWORD"))
	}
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("webdav HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ---- S3 (AWS Signature V4, works with AWS, MinIO, R2, B2, Wasabi) ----

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func s3Escape(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(s), "+", "%2B")
	}
	return strings.Join(parts, "/")
}

func uploadS3(ctx context.Context, cfg *config.Config, path, key string) error {
	bucket, region := cfg.Get("S3_BUCKET"), cfg.Get("S3_REGION")
	access, secret := cfg.Get("S3_ACCESS_KEY"), cfg.Get("S3_SECRET_KEY")
	if bucket == "" || access == "" || secret == "" {
		return errors.New("S3_BUCKET/S3_ACCESS_KEY/S3_SECRET_KEY obrigatórios")
	}
	endpoint := strings.TrimRight(cfg.Get("S3_ENDPOINT"), "/")
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	eu, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	host := eu.Host
	uriPath := "/" + s3Escape(bucket) + "/" + s3Escape(key)
	if !cfg.GetBool("S3_PATH_STYLE") {
		host = bucket + "." + eu.Host
		uriPath = "/" + s3Escape(key)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	now := time.Now().UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	canonicalHeaders := "host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	creq := strings.Join([]string{http.MethodPut, uriPath, "", canonicalHeaders, signed, payloadHash}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, sts))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, eu.Scheme+"://"+host+uriPath, f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Host = host
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", access, scope, signed, sig))
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
