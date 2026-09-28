package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/importer"
)

const importAccept = ".zip,.md,.markdown,.txt,.enex,.json,.jsonl,.ndjson,.html,.htm,.csv,.tsv,.vcf,.ics,.opml,.xml"

var importStates = map[string]string{
	importer.StateQueued: "na fila", importer.StateParsing: "lendo arquivos", importer.StateImporting: "importando",
	importer.StateLinking: "criando conexões", importer.StateDone: "concluída", importer.StateFailed: "falhou",
}

type importView struct {
	Formats []importer.FormatInfo
	Jobs    []importer.Report
	MaxMB   int64
	LLM     bool
	Accept  string
	Base    string
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	v := importView{Formats: importer.Formats, Jobs: s.Importer.Jobs(), MaxMB: s.Importer.MaxBytes() >> 20, LLM: s.LLM.Enabled(),
		Accept: importAccept, Base: s.Cfg.PublicURL()}
	s.render(w, "import", s.page(r, "Importar", "import", v))
}

func (s *Server) importJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Importer.Job(r.PathValue("id"))
	if !ok {
		http.Error(w, "importação não encontrada", http.StatusNotFound)
		return
	}
	s.fragment(w, "import_job", j.Report())
}

func importOptions(get func(string) string) importer.Options {
	on := func(k string) bool { v := get(k); return v == "true" || v == "1" || v == "on" }
	return importer.Options{Format: strings.TrimSpace(get("format")), LLM: on("llm"), Fetch: on("fetch"), Tags: splitTags(get("tags"))}
}

// saveUpload copies an upload into the importer's spool directory.
func (s *Server) saveUpload(src io.Reader, name string) (importer.File, error) {
	dir := s.Importer.UploadDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return importer.File{}, err
	}
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	dst := filepath.Join(dir, crypto.RandomToken(8)+strings.ToLower(filepath.Ext(name)))
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return importer.File{}, err
	}
	_, err = io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return importer.File{}, err
	}
	return importer.File{Path: dst, Name: name}, nil
}

func (s *Server) saveMultipart(files []*multipart.FileHeader) ([]importer.File, error) {
	var out []importer.File
	for _, fh := range files {
		src, err := fh.Open()
		if err != nil {
			removeFiles(out)
			return nil, err
		}
		f, err := s.saveUpload(src, fh.Filename)
		src.Close()
		if err != nil {
			removeFiles(out)
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func removeFiles(files []importer.File) {
	for _, f := range files {
		os.Remove(f.Path)
	}
}

func (s *Server) uploadError(err error) string {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return fmt.Sprintf("envio maior que o limite de %d MB (IMPORT_MAX_MB)", s.Importer.MaxBytes()>>20)
	}
	return "upload: " + err.Error()
}

func (s *Server) importUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.Importer.MaxBytes()+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		redirectFlash(w, r, "/import", s.uploadError(err), true)
		return
	}
	defer r.MultipartForm.RemoveAll()
	if len(r.MultipartForm.File["file"]) == 0 {
		redirectFlash(w, r, "/import", "selecione ao menos um arquivo", true)
		return
	}
	files, err := s.saveMultipart(r.MultipartForm.File["file"])
	if err != nil {
		redirectFlash(w, r, "/import", s.uploadError(err), true)
		return
	}
	j := s.Importer.Start(files, importOptions(r.FormValue), true)
	s.log.Info("importação iniciada", "job", j.Report().ID, "files", len(files))
	redirectFlash(w, r, "/import#job-"+j.Report().ID, "Importação iniciada — acompanhe o progresso abaixo.", false)
}

// apiImport imports synchronously: multipart field "file" (repeatable) or a raw body
// with ?filename=. Options: format, llm, fetch, tags (query or form).
func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.Importer.MaxBytes()+1<<20)
	var files []importer.File
	var err error
	get := r.URL.Query().Get
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err = r.ParseMultipartForm(32 << 20); err == nil {
			defer r.MultipartForm.RemoveAll()
			get = func(k string) string { return firstOr(r.FormValue(k), r.URL.Query().Get(k)) }
			files, err = s.saveMultipart(r.MultipartForm.File["file"])
		}
	} else {
		name := r.URL.Query().Get("filename")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "envie multipart (campo file) ou informe ?filename=arquivo.ext"})
			return
		}
		var f importer.File
		if f, err = s.saveUpload(r.Body, name); err == nil {
			files = []importer.File{f}
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": s.uploadError(err)})
		return
	}
	defer removeFiles(files)
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nenhum arquivo enviado"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Hour)
	defer cancel()
	rep := s.Importer.Run(ctx, files, importOptions(get))
	status := http.StatusOK
	if rep.State == importer.StateFailed {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, rep)
}
