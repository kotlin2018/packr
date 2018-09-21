package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"html/template"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gobuffalo/genny"
	"github.com/gobuffalo/genny/movinglater/gotools"
	"github.com/gobuffalo/packr/v2/file/resolver/encoding/hex"
	"github.com/gobuffalo/packr/v2/plog"

	"github.com/gobuffalo/packr/v2/jam/parser"
	"github.com/karrick/godirwalk"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var _ Store = &Disk{}

const DISK_GLOBAL_KEY = "__packr_global__"

type Disk struct {
	DBPath    string
	DBPackage string
	global    map[string]string
	boxes     map[string]*parser.Box
	moot      *sync.RWMutex
}

func NewDisk(path string, pkg string) *Disk {
	if len(path) == 0 {
		path = "packrd"
	}
	if len(pkg) == 0 {
		pkg = "packrd"
	}
	if !filepath.IsAbs(path) {
		path, _ = filepath.Abs(path)
	}
	return &Disk{
		DBPath:    path,
		DBPackage: pkg,
		global:    map[string]string{},
		boxes:     map[string]*parser.Box{},
		moot:      &sync.RWMutex{},
	}
}

func (d *Disk) FileNames(box *parser.Box) ([]string, error) {
	path := box.AbsPath
	if len(box.AbsPath) == 0 {
		path = box.Path
	}
	var names []string
	if _, err := os.Stat(path); err != nil {
		return names, nil
	}
	err := godirwalk.Walk(path, &godirwalk.Options{
		FollowSymbolicLinks: true,
		Callback: func(path string, de *godirwalk.Dirent) error {
			if !de.IsRegular() {
				return nil
			}
			names = append(names, path)
			return nil
		},
	})
	return names, err
}

func (d *Disk) Files(box *parser.Box) ([]*parser.File, error) {
	var files []*parser.File
	names, err := d.FileNames(box)
	if err != nil {
		return files, errors.WithStack(err)
	}
	for _, n := range names {
		b, err := ioutil.ReadFile(n)
		if err != nil {
			return files, errors.WithStack(err)
		}
		f := parser.NewFile(n, bytes.NewReader(b))
		files = append(files, f)
	}
	return files, nil
}

func (d *Disk) Pack(box *parser.Box) error {
	plog.Debug(d, "Pack", "box", box.Name)
	d.boxes[box.Name] = box
	names, err := d.FileNames(box)
	if err != nil {
		return errors.WithStack(err)
	}
	for _, n := range names {
		_, ok := d.global[n]
		if ok {
			continue
		}
		k := makeKey(box, n)
		// not in the global, so add it!
		d.global[n] = k
	}
	return nil
}

func (d *Disk) Clean(box *parser.Box) error {
	root := box.PackageDir
	if len(root) == 0 {
		return errors.New("can't clean an empty box.PackageDir")
	}
	plog.Debug(d, "Clean", "box", box.Name, "root", root)
	return Clean(root)
}

type options struct {
	Package     string
	GlobalFiles map[string]string
	Boxes       []optsBox
	GK          string
}

type optsBox struct {
	Name string
	Path string
}

func (d *Disk) Generator() (*genny.Generator, error) {
	g := genny.New()

	opts := options{
		Package:     d.DBPackage,
		GlobalFiles: map[string]string{},
		GK:          DISK_GLOBAL_KEY,
	}

	wg := errgroup.Group{}
	for k, v := range d.global {
		func(k, v string) {
			wg.Go(func() error {
				bb := &bytes.Buffer{}
				enc := hex.NewEncoder(bb)
				zw := gzip.NewWriter(enc)
				f, err := os.Open(k)
				if err != nil {
					return errors.WithStack(err)
				}
				defer f.Close()
				io.Copy(zw, f)
				if err := zw.Close(); err != nil {
					return errors.WithStack(err)
				}
				d.moot.Lock()
				opts.GlobalFiles[v] = bb.String()
				d.moot.Unlock()
				return nil
			})
		}(k, v)
	}

	if err := wg.Wait(); err != nil {
		return g, errors.WithStack(err)
	}

	for _, b := range d.boxes {
		ob := optsBox{
			Name: b.Name,
		}
		opts.Boxes = append(opts.Boxes, ob)
	}

	sort.Slice(opts.Boxes, func(a, b int) bool {
		return opts.Boxes[a].Name < opts.Boxes[b].Name
	})

	t := gotools.TemplateTransformer(opts, template.FuncMap{
		"printBox": func(ob optsBox) (template.HTML, error) {
			box := d.boxes[ob.Name]
			if box == nil {
				return "", errors.Errorf("could not find box %s", ob.Name)
			}
			fn, err := d.FileNames(box)
			if err != nil {
				return "", errors.WithStack(err)
			}
			if len(fn) == 0 {
				return "", nil
			}

			type file struct {
				Resolver    string
				ForwardPath string
			}

			gf := genny.NewFile("box.go.tmpl", strings.NewReader(diskGlobalBoxTmpl))
			var files []file
			for _, s := range fn {
				p := strings.TrimPrefix(s, box.AbsPath)
				p = strings.TrimPrefix(p, string(filepath.Separator))
				files = append(files, file{
					Resolver:    p,
					ForwardPath: makeKey(box, s),
				})
			}
			opts := map[string]interface{}{
				"Box":   ob,
				"Files": files,
			}
			t := gotools.TemplateTransformer(opts, nil)
			gf, err = t.Transform(gf)
			if err != nil {
				return "", errors.WithStack(err)
			}
			return template.HTML(gf.String()), nil
		},
	})
	g.Transformer(t)
	fp := filepath.Join(d.DBPath, "packed-packr.go.tmpl")
	g.File(genny.NewFile(fp, strings.NewReader(diskGlobalTmpl)))

	ip := filepath.Dir(d.DBPath)
	ip = strings.TrimPrefix(ip, filepath.Join(GoPath(), "src"))
	ip = strings.TrimPrefix(ip, string(filepath.Separator))
	ip = path.Join(ip, d.DBPackage)

	for _, n := range opts.Boxes {
		b := d.boxes[n.Name]
		if b == nil {
			continue
		}
		p := filepath.Join(b.PackageDir, b.Package+"-packr.go.tmpl")
		f := genny.NewFile(p, strings.NewReader(diskImportTmpl))

		o := struct {
			Package string
			Import  string
		}{
			Package: b.Package,
			Import:  ip,
		}

		t := gotools.TemplateTransformer(o, template.FuncMap{})
		f, err := t.Transform(f)
		if err != nil {
			return g, nil
		}
		g.File(f)
	}

	return g, nil
}

func (d *Disk) Close() error {
	plog.Debug(d, "Close")
	run := genny.WetRunner(context.Background())
	run.Logger = plog.Default
	if err := run.WithNew(d.Generator()); err != nil {
		return errors.WithStack(err)
	}
	return run.Run()
}

// resolve file paths (only) for the boxes
// compile "global" db
// resolve files for boxes to point at global db
// write global db to disk (default internal/packr)
// write boxes db to disk (default internal/packr)
// write -packr.go files in each package (1 per package) that init the global db

func makeKey(box *parser.Box, path string) string {
	// return path
	// s := strings.TrimPrefix(path, box.AbsPath)
	// return strings.TrimPrefix(s, string(filepath.Separator))
	hasher := md5.New()
	hasher.Write([]byte(path))
	return hex.EncodeToString(hasher.Sum(nil))
}