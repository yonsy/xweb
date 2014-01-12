package xweb

import (
	"errors"
	"net/http"
	"os"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type handler interface {
	Do(http.ResponseWriter, *http.Request) error
}

type staticHandler struct {
	app *App
}

func (h *staticHandler) Do(w http.ResponseWriter, req *http.Request) error {
	newPath := req.URL.Path[len(h.app.BasePath):]
	a := h.app

	staticFile := path.Join(a.AppConfig.StaticDir, newPath)

	isStaticFileToCompress := false
	if a.Server.Config.EnableGzip && a.Server.Config.StaticExtensionsToGzip != nil && len(a.Server.Config.StaticExtensionsToGzip) > 0 {
		for _, statExtension := range a.Server.Config.StaticExtensionsToGzip {
			if strings.HasSuffix(strings.ToLower(staticFile), strings.ToLower(statExtension)) {
				isStaticFileToCompress = true
				break
			}
		}
	}

	if isStaticFileToCompress {
		finfo, err := os.Stat(staticFile)
		if err != nil {
			return err
		}

		if finfo.IsDir() {
			return errors.New("unsupported serve dir")
		}

		a.ContentEncoding = GetAcceptEncodingZip(req)
		memzipfile, err := OpenMemZipFile(staticFile, a.ContentEncoding)
		if err != nil {
			return err
		}
		a.InitHeadContent(w, finfo.Size())
		http.ServeContent(w, req, staticFile, finfo.ModTime(), memzipfile)
	} else {
		http.ServeFile(w, req, staticFile)
	}
	return nil
}

type actionHandler struct {
	app     *App
	methods map[string]bool
	cr      *regexp.Regexp
	ctype   reflect.Type
	handler string
}

func (h *actionHandler) Do(w http.ResponseWriter, req *http.Request) error {
	return h.DoCr(w, req, make([]string, 0))
}

func (h *actionHandler) DoCr(w http.ResponseWriter, req *http.Request, match []string) error {
	//log the request
	//var logEntry bytes.Buffer
	a := h.app

	//ignore errors from ParseForm because it's usually harmless.
	ct := req.Header.Get("Content-Type")
	if strings.Contains(ct, "multipart/form-data") {
		req.ParseMultipartForm(a.AppConfig.MaxUploadSize)
	} else {
		req.ParseForm()
	}

	//Set the default content-type
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	/*if !a.filter(w, req) {
		return
	}*/

	//requestPath := req.URL.Path

	if a.AppConfig.CheckXrsf && req.Method == "POST" {
		res, err := req.Cookie(XSRF_TAG)
		formVals := req.Form[XSRF_TAG]
		var formVal string
		if len(formVals) > 0 {
			formVal = formVals[0]
		}
		if err != nil || res.Value == "" || res.Value != formVal {
			w.WriteHeader(500)
			w.Write([]byte("xrsf error."))
			return nil
		}
	}

	var args []reflect.Value
	for _, arg := range match {
		args = append(args, reflect.ValueOf(arg))
	}
	vc := reflect.New(h.ctype)
	c := Action{Request: req, App: a, ResponseWriter: w, T: T{}, f: T{}}
	for k, v := range a.VarMaps {
		c.T[k] = v
	}
	fieldA := vc.Elem().FieldByName("Action")
	if fieldA.IsValid() {
		fieldA.Set(reflect.ValueOf(c))
	}

	fieldC := vc.Elem().FieldByName("C")
	if fieldC.IsValid() {
		fieldC.Set(reflect.ValueOf(vc))
	}

	a.StructMap(vc.Elem(), req)

	initM := vc.MethodByName("Init")
	if initM.IsValid() {
		params := []reflect.Value{}
		initM.Call(params)
	}

	//[SWH|+]------------------------------------------Before-Hook
	structName := reflect.ValueOf(h.ctype.String())
	actionName := reflect.ValueOf(h.handler)
	structAction := []reflect.Value{structName, actionName}
	initM = vc.MethodByName("Before")
	if initM.IsValid() {
		initM.Call(structAction)
	}

	ret, err := a.safelyCall(vc, h.handler, args)
	if err != nil {
		c.GetLogger().Println(err)
		//there was an error or panic while calling the handler
		c.Abort(500, "Server Error")
		return nil
	}

	//[SWH|+]------------------------------------------After-Hook
	initM = vc.MethodByName("After")
	if initM.IsValid() {
		actionResult := reflect.ValueOf(ret)
		structAction = []reflect.Value{structName, actionName, actionResult}
		initM.Call(structAction)
	}

	if len(ret) == 0 {
		return nil
	}

	sval := ret[0]

	var content []byte
	if sval.Kind() == reflect.String {
		content = []byte(sval.String())
	} else if sval.Kind() == reflect.Slice && sval.Type().Elem().Kind() == reflect.Uint8 {
		content = sval.Interface().([]byte)
	} else if e, ok := sval.Interface().(error); ok && e != nil {
		c.GetLogger().Println(e)
		c.Abort(500, "Server Error")
		return nil
	}

	c.SetHeader("Content-Length", strconv.Itoa(len(content)))
	_, err = c.ResponseWriter.Write(content)
	if err != nil {
		a.Logger.Println("Error during write: ", err)
	}
	return nil
}

type errorHandler struct {
	app *App
	err *AbortError
}

type Routes struct {
	app          *App
	mapRoutes    map[string]handler
	rgRoutes     []*actionHandler
	defaultIndex []string
}

func NewRoutes(app *App, defaultHome []string) *Routes {
	return &Routes{app, make(map[string]handler),
		make([]*actionHandler, 0), defaultHome,
	}
}

func (r *Routes) addStatic(s string, handler handler) error {
	r.mapRoutes[s] = handler
	return nil
}

func NewActionHandler(app *App, ctype reflect.Type, handler string) *actionHandler {
	return &actionHandler{app: app, ctype: ctype, handler: handler}
}

// there are two kind of routes. one is accurate route, we use map. another is
// regex route, we use slice.
func (r *Routes) addAction(s string, methods map[string]bool, ctype reflect.Type, actionName string) error {
	handler := &actionHandler{app: r.app, methods: methods, ctype: ctype, handler: actionName}

	if !strings.ContainsAny(s, "*?") {
		r.mapRoutes[s] = handler
		return nil
	}

	cr, err := regexp.Compile(s)
	if err != nil {
		return err
	}

	handler.cr = cr
	r.rgRoutes = append(r.rgRoutes, handler)
	return nil
}

func (r *Routes) handle(req *http.Request, w http.ResponseWriter) {
	var method string
	method = req.Method
	if method == "HEAD" {
		method = "GET"
	}

	//set some default headers
	w.Header().Set("Server", "xweb")
	tm := time.Now().UTC()
	w.Header().Set("Date", webTime(tm))

	// search for accurate maps
	requestPath := req.URL.Path
	if handler, ok := r.mapRoutes[requestPath]; ok {
		handler.Do(w, req)
		return
	}

	// range for unaccurate slice
	for _, handler := range r.rgRoutes {
		if !handler.cr.MatchString(requestPath) {
			continue
		}

		match := handler.cr.FindStringSubmatch(requestPath)
		if len(match[0]) != len(requestPath) {
			continue
		}

		handler.DoCr(w, req, match[1:])
		return
	}

	// test if default html page exists.
	for _, page := range r.defaultIndex {
		idxPath := path.Join(requestPath, page)
		if handler, ok := r.mapRoutes[idxPath]; ok {
			handler.Do(w, req)
			return
		}
	}

	// if there is not, then return 404
	//notFound(req, w)
}
