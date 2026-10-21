//go:build windows

// ИванКод — запускатель веб-программ (WebView2 / Edge --app / браузер).
// Файл = этот EXE + ZIP (ivrun.json + www/...) + хвост 24 байта.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const magic = "IVRUNv01"

type cfg struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Exe     string `json:"exe"`
	Version string `json:"version"`
	Width   uint   `json:"width"`
	Height  uint   `json:"height"`
	Publisher string `json:"publisher"`
}

var (
	user32      = windows.NewLazySystemDLL("user32.dll")
	msgBoxW     = user32.NewProc("MessageBoxW")
	silent      bool
	logf        *os.File
)

func say(title, text string, flags uintptr) int {
	if silent {
		return 6
	}
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	r, _, _ := msgBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
	return int(r)
}
func lg(f string, a ...interface{}) {
	if logf != nil {
		fmt.Fprintf(logf, f+"\n", a...)
	}
}

func readSelf() (exe string, data []byte, zr *zip.Reader, mode byte, c cfg, err error) {
	exe, err = os.Executable()
	if err != nil {
		return
	}
	data, err = os.ReadFile(exe)
	if err != nil {
		return
	}
	if len(data) < 24 || string(data[len(data)-24:len(data)-16]) != magic {
		err = errors.New("в программе нет встроенных файлов (повреждена)")
		return
	}
	zl := binary.LittleEndian.Uint64(data[len(data)-16:])
	mode = data[len(data)-8]
	end := uint64(len(data) - 24)
	if zl > end {
		err = errors.New("повреждён встроенный архив")
		return
	}
	zb := data[end-zl : end]
	zr, err = zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		return
	}
	f, e := zr.Open("ivrun.json")
	if e != nil {
		err = e
		return
	}
	b, _ := io.ReadAll(f)
	f.Close()
	err = json.Unmarshal(b, &c)
	if c.ID == "" {
		c.ID = "app"
	}
	if c.Name == "" {
		c.Name = c.ID
	}
	if c.Exe == "" {
		c.Exe = c.ID
	}
	return
}

func base(c cfg) string {
	l := os.Getenv("LOCALAPPDATA")
	if l == "" {
		l = os.TempDir()
	}
	return filepath.Join(l, "IvanCodeApps", c.ID)
}

// распаковка www в папку по контрольной сумме (повторно не распаковывает)
func extract(zr *zip.Reader, c cfg, sum uint32) (string, error) {
	root := base(c)
	dir := filepath.Join(root, fmt.Sprintf("www-%08x", sum))
	if _, e := os.Stat(filepath.Join(dir, ".ok")); e == nil {
		return dir, nil
	}
	os.RemoveAll(dir)
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "www/") || strings.HasSuffix(f.Name, "/") || strings.Contains(f.Name, "..") {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(f.Name, "www/")))
		os.MkdirAll(filepath.Dir(p), 0755)
		r, e := f.Open()
		if e != nil {
			return "", e
		}
		w, e := os.Create(p)
		if e != nil {
			r.Close()
			return "", e
		}
		_, e = io.Copy(w, r)
		r.Close()
		w.Close()
		if e != nil {
			return "", e
		}
	}
	os.WriteFile(filepath.Join(dir, ".ok"), []byte("1"), 0644)
	// старые версии
	if ents, e := os.ReadDir(root); e == nil {
		for _, en := range ents {
			if strings.HasPrefix(en.Name(), "www-") && filepath.Join(root, en.Name()) != dir {
				os.RemoveAll(filepath.Join(root, en.Name()))
			}
		}
	}
	return dir, nil
}

func port(c cfg) int { return 20000 + int(crc32.ChecksumIEEE([]byte(c.ID))%20000) }

func serve(dir string, c cfg) (string, error) {
	mime.AddExtensionType(".js", "text/javascript")
	p := port(c)
	var ln net.Listener
	var err error
	for i := 0; i < 30; i++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+i))
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", err
	}
	go http.Serve(ln, http.FileServer(http.Dir(dir)))
	return "http://" + ln.Addr().String() + "/", nil
}

func findEdge() string {
	for _, v := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		d := os.Getenv(v)
		if d == "" {
			continue
		}
		p := filepath.Join(d, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, e := os.Stat(p); e == nil {
			return p
		}
	}
	return ""
}

func runFallback(dir string, c cfg, why string) {
	url, err := serve(dir, c)
	if err != nil {
		say(c.Name, "Не удалось запустить программу: "+err.Error(), 0x10)
		return
	}
	lg("fallback %s url=%s", why, url)
	fmt.Println("IVRUN-URL " + url)
	if os.Getenv("IVRUN_NOBROWSER") != "" {
		say(c.Name, "Сервер программы: "+url, 0x40)
		select {}
	}
	if e := findEdge(); e != "" {
		cmd := exec.Command(e, "--app="+url, "--user-data-dir="+filepath.Join(base(c), "edge"), "--no-first-run", "--no-default-browser-check", fmt.Sprintf("--window-size=%d,%d", c.Width, c.Height))
		if cmd.Start() == nil {
			cmd.Wait()
			return
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	say(c.Name, "«"+c.Name+"» открыта в браузере.\n\nДля работы в отдельном окне установите Microsoft Edge WebView2 (бесплатно): https://go.microsoft.com/fwlink/p/?LinkId=2124703\n\nНажмите ОК, чтобы закрыть программу.", 0x40)
}

func runApp(dir string, c cfg) {
	v, verr := webviewloader.GetInstalledVersion()
	if os.Getenv("IVRUN_FORCE_FALLBACK") != "" || verr != nil || v == "" {
		runFallback(dir, c, fmt.Sprintf("webview2=%q err=%v", v, verr))
		return
	}
	if c.Width == 0 {
		c.Width = 1100
	}
	if c.Height == 0 {
		c.Height = 760
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(base(c), "data"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{Title: c.Name, Width: c.Width, Height: c.Height, IconId: 1, Center: true},
	})
	if w == nil {
		runFallback(dir, c, "webview2 create failed")
		return
	}
	defer w.Destroy()
	type chr interface{ Chromium() *edge.Chromium }
	ch := w.(chr).Chromium()
	ok := false
	if ch != nil {
		if s, e := ch.GetSettings(); e == nil {
			s.PutAreDefaultContextMenusEnabled(true)
		}
		if w3 := ch.GetICoreWebView2_3(); w3 != nil {
			if e := w3.SetVirtualHostNameToFolderMapping("app.ivancode", dir, edge.COREWEBVIEW2_HOST_RESOURCE_ACCESS_KIND_ALLOW); e == nil {
				ok = true
			}
		}
	}
	if ok {
		w.Navigate("https://app.ivancode/index.html")
	} else {
		url, err := serve(dir, c)
		if err != nil {
			say(c.Name, "Не удалось запустить: "+err.Error(), 0x10)
			return
		}
		w.Navigate(url + "index.html")
	}
	w.Run()
}

// ---------- установщик ----------
func shortcut(lnk, target, dir string) error {
	ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED|ole.COINIT_SPEED_OVER_MEMORY)
	defer ole.CoUninitialize()
	u, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return err
	}
	defer u.Release()
	ws, err := u.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer ws.Release()
	cs, err := oleutil.CallMethod(ws, "CreateShortcut", lnk)
	if err != nil {
		return err
	}
	d := cs.ToIDispatch()
	defer d.Release()
	oleutil.PutProperty(d, "TargetPath", target)
	oleutil.PutProperty(d, "WorkingDirectory", dir)
	oleutil.PutProperty(d, "IconLocation", target+",0")
	_, err = oleutil.CallMethod(d, "Save")
	return err
}

func known(id *windows.KNOWNFOLDERID, env, sub string) string {
	if p, e := windows.KnownFolderPath(id, 0); e == nil && p != "" {
		return p
	}
	return filepath.Join(os.Getenv(env), sub)
}

func setup(self string, data []byte, c cfg) int {
	if say("Установка «"+c.Name+"»", "Установить программу «"+c.Name+"» на этот компьютер?\n\nБудет создан ярлык на рабочем столе и в меню «Пуск».", 0x24) != 6 {
		return 1
	}
	dest := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", c.ID)
	if err := os.MkdirAll(dest, 0755); err != nil {
		say(c.Name, "Не удалось создать папку "+dest+": "+err.Error(), 0x10)
		return 2
	}
	exe := filepath.Join(dest, c.Exe+".exe")
	b := append([]byte{}, data...)
	b[len(b)-8] = 'A'
	if err := os.WriteFile(exe, b, 0755); err != nil {
		say(c.Name, "Не удалось записать "+exe+": "+err.Error()+"\n\nЕсли программа открыта — закройте её и запустите установку снова.", 0x10)
		return 3
	}
	desk := known(windows.FOLDERID_Desktop, "USERPROFILE", "Desktop")
	progs := known(windows.FOLDERID_Programs, "APPDATA", `Microsoft\Windows\Start Menu\Programs`)
	var warn []string
	for _, l := range []string{filepath.Join(desk, c.Name+".lnk"), filepath.Join(progs, c.Name+".lnk")} {
		if e := shortcut(l, exe, dest); e != nil {
			warn = append(warn, l+": "+e.Error())
			lg("shortcut %s: %v", l, e)
		}
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\IvanCode-`+c.ID, registry.ALL_ACCESS)
	if err == nil {
		k.SetStringValue("DisplayName", c.Name)
		k.SetStringValue("DisplayVersion", c.Version)
		k.SetStringValue("Publisher", c.Publisher)
		k.SetStringValue("DisplayIcon", exe)
		k.SetStringValue("InstallLocation", dest)
		k.SetStringValue("UninstallString", `"`+exe+`" --uninstall`)
		k.SetStringValue("QuietUninstallString", `"`+exe+`" --uninstall /S`)
		k.SetDWordValue("NoModify", 1)
		k.SetDWordValue("NoRepair", 1)
		k.Close()
	}
	lg("installed %s", exe)
	fmt.Println("IVRUN-INSTALLED " + exe)
	if !silent {
		msg := "Готово! «" + c.Name + "» установлена.\nЯрлык — на рабочем столе."
		if len(warn) > 0 {
			msg = "«" + c.Name + "» установлена в " + dest + ", но ярлык создать не удалось."
		}
		say(c.Name, msg+"\n\nЗапустить сейчас?", 0x44)
		exec.Command(exe).Start()
	}
	return 0
}

func uninstall(self string, c cfg) int {
	if say("Удаление «"+c.Name+"»", "Удалить программу «"+c.Name+"»?\n(Ваши данные останутся.)", 0x24) != 6 {
		return 1
	}
	desk := known(windows.FOLDERID_Desktop, "USERPROFILE", "Desktop")
	progs := known(windows.FOLDERID_Programs, "APPDATA", `Microsoft\Windows\Start Menu\Programs`)
	os.Remove(filepath.Join(desk, c.Name+".lnk"))
	os.Remove(filepath.Join(progs, c.Name+".lnk"))
	registry.DeleteKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\IvanCode-`+c.ID)
	dir := filepath.Dir(self)
	cmd := exec.Command("cmd", "/c", "ping -n 3 127.0.0.1 >nul & rmdir /s /q \""+dir+"\"")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Start()
	say(c.Name, "Программа удалена.", 0x40)
	return 0
}

func main() {
	for _, a := range os.Args[1:] {
		if strings.EqualFold(a, "/S") || a == "--silent" {
			silent = true
		}
	}
	self, data, zr, mode, c, err := readSelf()
	if err != nil {
		say("ИванКод", "Не удалось запустить: "+err.Error(), 0x10)
		os.Exit(10)
	}
	os.MkdirAll(base(c), 0755)
	logf, _ = os.OpenFile(filepath.Join(base(c), "run.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	for _, a := range os.Args[1:] {
		if a == "--uninstall" {
			os.Exit(uninstall(self, c))
		}
		if a == "--install" {
			mode = 'S'
		}
	}
	if mode == 'S' {
		os.Exit(setup(self, data, c))
	}
	dir, err := extract(zr, c, crc32.ChecksumIEEE(data[:len(data)-24]))
	if err != nil {
		say(c.Name, "Не удалось распаковать файлы программы: "+err.Error(), 0x10)
		os.Exit(11)
	}
	lg("www=%s", dir)
	for _, a := range os.Args[1:] {
		if a == "--extract-only" {
			fmt.Println("IVRUN-WWW " + dir)
			return
		}
	}
	runApp(dir, c)
}
