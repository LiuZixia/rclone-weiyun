// Package weiyun provides an interface to Tencent Weiyun.
package weiyun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const (
	minSleep = 20 * time.Millisecond
	maxSleep = 2 * time.Second
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "weiyun",
		Description: "Tencent Weiyun",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "cookie",
			Help:      "Cookie used for Weiyun authentication.",
			Sensitive: true,
		}, {
			Name:    "base_url",
			Help:    "Weiyun API endpoint.",
			Default: "https://api.weiyun.com",
		}, {
			Name:     "user_agent",
			Help:     "Custom User-Agent for Weiyun requests.",
			Advanced: true,
			Default:  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		}},
	})
}

type Options struct {
	Cookie    string `config:"cookie"`
	BaseURL   string `config:"base_url"`
	UserAgent string `config:"user_agent"`
}

type Fs struct {
	name     string
	root     string
	opt      Options
	srv      *rest.Client
	pacer    *fs.Pacer
	features *fs.Features
}

type Object struct {
	fs      *Fs
	remote  string
	size    int64
	sha1    string
	modTime time.Time
}

type apiResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type fileItem struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	FileID   string `json:"file_id"`
	IsDir    bool   `json:"is_dir"`
	Size     int64  `json:"size"`
	SHA1     string `json:"sha1"`
	ModifyAt int64  `json:"modify_time"`
	CreateAt int64  `json:"create_time"`
}

type listResponse struct {
	List []fileItem `json:"list"`
}

func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(opt.BaseURL, "/")
	f := &Fs{
		name:  name,
		root:  strings.Trim(path.Clean(root), "/"),
		opt:   *opt,
		srv:   rest.NewClient(fshttp.NewClient(ctx)).SetRoot(baseURL),
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep))),
	}
	if opt.Cookie != "" {
		f.srv.SetHeader("Cookie", opt.Cookie)
	}
	if opt.UserAgent != "" {
		f.srv.SetHeader("User-Agent", opt.UserAgent)
	}
	f.features = (&fs.Features{}).Fill(ctx, f)
	if f.root == "." {
		f.root = ""
	}
	return f, nil
}

func (f *Fs) Name() string             { return f.name }
func (f *Fs) Root() string             { return f.root }
func (f *Fs) String() string           { return "weiyun:" + f.root }
func (f *Fs) Precision() time.Duration { return time.Second }
func (f *Fs) Hashes() hash.Set         { return hash.Set(hash.SHA1) }
func (f *Fs) Features() *fs.Features   { return f.features }

func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	items, err := f.list(ctx, f.absPath(dir))
	if err != nil {
		return nil, err
	}
	entries := make(fs.DirEntries, 0, len(items))
	for _, item := range items {
		remote := f.trimRoot(item.Path)
		if remote == "" || remote == "." {
			continue
		}
		if item.IsDir {
			entries = append(entries, fs.NewDir(remote, time.Unix(item.ModifyAt, 0)))
		} else {
			entries = append(entries, &Object{fs: f, remote: remote, size: item.Size, sha1: item.SHA1, modTime: time.Unix(item.ModifyAt, 0)})
		}
	}
	return entries, nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	parent, leaf := path.Split(f.absPath(remote))
	items, err := f.list(ctx, strings.TrimSuffix(parent, "/"))
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if !item.IsDir && item.Name == leaf {
			return &Object{fs: f, remote: f.trimRoot(item.Path), size: item.Size, sha1: item.SHA1, modTime: time.Unix(item.ModifyAt, 0)}, nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	if err := f.upload(ctx, f.absPath(remote), in); err != nil {
		return nil, err
	}
	return f.NewObject(ctx, remote)
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return fs.ErrorNotImplemented
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return fs.ErrorNotImplemented
}

func (f *Fs) list(ctx context.Context, p string) ([]fileItem, error) {
	params := url.Values{}
	params.Set("path", "/"+strings.Trim(p, "/"))
	opts := rest.Opts{Method: http.MethodGet, Path: "/api/v3/disk/list", Parameters: params}
	resp := &apiResponse{}
	if err := f.callJSON(ctx, &opts, nil, resp); err != nil {
		return nil, err
	}
	data := &listResponse{}
	if len(resp.Data) != 0 {
		if err := json.Unmarshal(resp.Data, data); err != nil {
			return nil, fmt.Errorf("decode list response: %w", err)
		}
	}
	return data.List, nil
}

func (f *Fs) upload(ctx context.Context, p string, in io.Reader) error {
	opts := rest.Opts{Method: http.MethodPost, Path: "/api/v3/disk/upload", MultipartContentName: "file", MultipartFileName: path.Base(p), Body: in}
	opts.ExtraHeaders = map[string]string{"X-Path": "/" + strings.Trim(p, "/")}
	return f.callJSON(ctx, &opts, nil, &apiResponse{})
}

func (f *Fs) deleteFile(ctx context.Context, p string) error {
	body := map[string]string{"path": "/" + strings.Trim(p, "/")}
	opts := rest.Opts{Method: http.MethodPost, Path: "/api/v3/disk/delete"}
	return f.callJSON(ctx, &opts, body, &apiResponse{})
}

func (f *Fs) downloadURL(ctx context.Context, p string) (string, error) {
	params := url.Values{}
	params.Set("path", "/"+strings.Trim(p, "/"))
	opts := rest.Opts{Method: http.MethodGet, Path: "/api/v3/disk/download", Parameters: params}
	resp := &apiResponse{}
	if err := f.callJSON(ctx, &opts, nil, resp); err != nil {
		return "", err
	}
	var v struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(resp.Data, &v); err != nil {
		return "", fmt.Errorf("decode download response: %w", err)
	}
	if v.URL == "" {
		return "", fs.ErrorObjectNotFound
	}
	return v.URL, nil
}

func (f *Fs) callJSON(ctx context.Context, opts *rest.Opts, req, out any) error {
	return f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, opts, req, out)
		if err != nil {
			return f.shouldRetry(ctx, resp, err)
		}
		if ap, ok := out.(*apiResponse); ok && ap.Code != 0 {
			if ap.Code == 404 {
				return false, fs.ErrorObjectNotFound
			}
			return false, fmt.Errorf("weiyun api error code=%d msg=%s", ap.Code, ap.Msg)
		}
		return false, nil
	})
}

func (f *Fs) shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, []int{429, 500, 502, 503, 504}), err
}

func (f *Fs) absPath(remote string) string {
	remote = strings.Trim(remote, "/")
	if f.root == "" {
		return remote
	}
	if remote == "" {
		return f.root
	}
	return path.Join(f.root, remote)
}

func (f *Fs) trimRoot(p string) string {
	p = strings.Trim(strings.TrimPrefix(p, "/"), "/")
	if f.root == "" {
		return p
	}
	prefix := strings.Trim(f.root, "/") + "/"
	if strings.HasPrefix(p, prefix) {
		return strings.TrimPrefix(p, prefix)
	}
	if p == strings.Trim(f.root, "/") {
		return ""
	}
	return p
}

func (o *Object) Fs() fs.Info                           { return o.fs }
func (o *Object) String() string                        { return o.remote }
func (o *Object) Remote() string                        { return o.remote }
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }
func (o *Object) Size() int64                           { return o.size }

func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	if ty != hash.SHA1 {
		return "", hash.ErrUnsupported
	}
	return o.sha1, nil
}

func (o *Object) SetModTime(ctx context.Context, t time.Time) error { return fs.ErrorCantSetModTime }
func (o *Object) Storable() bool                                    { return true }

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	u, err := o.fs.downloadURL(ctx, o.fs.absPath(o.remote))
	if err != nil {
		return nil, err
	}
	opts := rest.Opts{Method: http.MethodGet, RootURL: u, Options: options}
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		var e error
		resp, e = o.fs.srv.Call(ctx, &opts)
		return o.fs.shouldRetry(ctx, resp, e)
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if err := o.fs.upload(ctx, o.fs.absPath(o.remote), in); err != nil {
		return err
	}
	o.size = src.Size()
	o.modTime = src.ModTime(ctx)
	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	return o.fs.deleteFile(ctx, o.fs.absPath(o.remote))
}

var (
	_ fs.Fs     = (*Fs)(nil)
	_ fs.Object = (*Object)(nil)
)
