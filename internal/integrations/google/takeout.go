package google

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
)

const takeoutCursorKey = "google.takeout.cursor"

// exportPrefix groups the parts of one export: takeout-20260928T101010Z-001.zip → takeout-20260928T101010Z.
func exportPrefix(name string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if i := strings.LastIndex(name, "-"); i > 0 {
		return name[:i]
	}
	return name
}

// latestExport keeps only the parts of the most recent export.
func latestExport(files []agent.DriveFile) []agent.DriveFile {
	newest := files[0]
	for _, f := range files {
		if f.Created.After(newest.Created) {
			newest = f
		}
	}
	want := exportPrefix(newest.Name)
	var out []agent.DriveFile
	for _, f := range files {
		if exportPrefix(f.Name) == want {
			out = append(out, f)
		}
	}
	return out
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// SyncTakeout downloads Google Takeout archives saved to Drive (scheduled exports) and
// queues their import. The first run takes only the most recent export.
func (s *Syncer) SyncTakeout(ctx context.Context) (int, error) {
	if s.ImportFile == nil || !s.g.Can(agent.GoogleDrive) {
		return 0, nil
	}
	db := s.ag.DB()
	var cursor time.Time
	if v, ok, _ := db.KVGet(ctx, takeoutCursorKey); ok {
		cursor, _ = time.Parse(time.RFC3339Nano, v)
	}
	files, err := s.g.TakeoutArchives(ctx, cursor)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	if cursor.IsZero() {
		files = latestExport(files)
	}
	dir := filepath.Join(s.g.cfg.GetPath("DATA_DIR"), "takeout")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(2)
	for _, f := range files {
		g.Go(func() error {
			path := filepath.Join(dir, "drive-"+safeName(f.ID)+".zip")
			if st, err := os.Stat(path); err != nil || (f.Size > 0 && st.Size() != f.Size) {
				s.g.log.Info("baixando Google Takeout do Drive", "file", f.Name, "mb", f.Size>>20)
				if err := s.download(gctx, f.ID, path); err != nil {
					return err
				}
			}
			return s.ImportFile(gctx, path, f.Name)
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}
	last := files[0].Created
	for _, f := range files {
		if f.Created.After(last) {
			last = f.Created
		}
	}
	return len(files), db.KVSet(ctx, takeoutCursorKey, last.UTC().Format(time.RFC3339Nano))
}

func (s *Syncer) download(ctx context.Context, id, path string) error {
	tmp := path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = s.g.Download(ctx, id, out)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
