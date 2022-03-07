package codegen

import (
	"bufio"
	"bytes"
	"github.com/sirupsen/logrus"
	"github.com/unionj-cloud/go-doudou/cmd/internal/astutils"
	"github.com/unionj-cloud/go-doudou/toolkit/copier"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

var iclientTmpl = `package client

import (
	"context"
	"github.com/go-resty/resty/v2"
	"{{.VoPackage}}"
	v3 "github.com/unionj-cloud/go-doudou/toolkit/openapi/v3"
	"os"
)

type I{{.Meta.Name}}Client interface {
{{- range $m := .Meta.Methods }}
	{{$m.Name}}(ctx context.Context, _headers map[string]string, {{- range $i, $p := $m.Params}}
	{{- if ne $p.Type "context.Context" }}
	{{- $p.Name}} {{$p.Type}},
	{{- end }}
    {{- end }}) (_resp *resty.Response, {{- range $i, $r := $m.Results}}
                     {{- if $i}},{{end}}
                     {{- $r.Name}} {{$r.Type}}
                     {{- end }})
{{- end }}
}
`

// GenGoIClient generates golang http client interface code from result of parsing svc.go file in project root path
func GenGoIClient(dir string, ic astutils.InterfaceCollector) {
	var (
		err        error
		clientfile string
		f          *os.File
		tpl        *template.Template
		sqlBuf     bytes.Buffer
		clientDir  string
		fi         os.FileInfo
		source     string
		modfile    string
		modName    string
		firstLine  string
		modf       *os.File
		meta       astutils.InterfaceMeta
	)
	clientDir = filepath.Join(dir, "client")
	if err = os.MkdirAll(clientDir, os.ModePerm); err != nil {
		panic(err)
	}

	clientfile = filepath.Join(clientDir, "iclient.go")
	fi, err = os.Stat(clientfile)
	if err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	if fi != nil {
		logrus.Warningln("file iclient.go will be overwritten")
	}
	if f, err = os.Create(clientfile); err != nil {
		panic(err)
	}
	defer f.Close()

	err = copier.DeepCopy(ic.Interfaces[0], &meta)
	if err != nil {
		panic(err)
	}

	modfile = filepath.Join(dir, "go.mod")
	if modf, err = os.Open(modfile); err != nil {
		panic(err)
	}
	reader := bufio.NewReader(modf)
	if firstLine, err = reader.ReadString('\n'); err != nil {
		panic(err)
	}
	modName = strings.TrimSpace(strings.TrimPrefix(firstLine, "module"))

	if tpl, err = template.New("iclient.go.tmpl").Parse(iclientTmpl); err != nil {
		panic(err)
	}
	if err = tpl.Execute(&sqlBuf, struct {
		VoPackage string
		Meta      astutils.InterfaceMeta
	}{
		VoPackage: modName + "/vo",
		Meta:      meta,
	}); err != nil {
		panic(err)
	}

	source = strings.TrimSpace(sqlBuf.String())
	astutils.FixImport([]byte(source), clientfile)
}