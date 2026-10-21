module ivrunner

go 1.21

require (
	github.com/go-ole/go-ole v1.3.0
	github.com/jchv/go-webview2 v0.0.0
	golang.org/x/sys v0.25.0
)

require github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect

replace github.com/jchv/go-webview2 => ./gowv
