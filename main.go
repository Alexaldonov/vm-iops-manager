package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
)

// index.html содержит весь интерфейс (HTML/CSS/JS одним файлом) и вшивается
// в бинарник на этапе сборки.
//
// index.html contains the entire UI (HTML/CSS/JS in one file), embedded
// into the binary at build time.
//
//go:embed web/index.html
var indexHTML string

// version — версия сборки; отражается в имени exe-файла (vm-iops-manager-vX.Y.exe)
// и выводится в консоль при старте.
//
// version — build version; shown in the exe filename (vm-iops-manager-vX.Y.exe)
// and printed to the console on startup.
const version = "0.9"

// generateToken создаёт одноразовый случайный токен авторизации для текущего запуска.
//
// generateToken creates a one-time random auth token for this run.
func generateToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// openBrowser открывает url в браузере по умолчанию для текущей ОС.
//
// openBrowser opens url in the system's default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("не удалось открыть локальный порт: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	token := generateToken()
	url := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", port, token)

	mgr := NewManager()
	api := newAPIServer(mgr, token)

	mux := http.NewServeMux()
	api.routes(mux)

	page := strings.Replace(indexHTML, "__TOKEN__", token, 1)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})

	// net.Listen на 127.0.0.1 уже ограничивает доступ loopback-ом; проверка
	// Host дополнительно защищает от DNS-rebinding с чужим Origin.
	//
	// net.Listen on 127.0.0.1 already restricts access to loopback; the Host
	// check additionally guards against DNS-rebinding from a foreign Origin.
	guarded := hostGuard(mux)

	fmt.Println("VM IOPS Manager v" + version + " запущен.")
	fmt.Println("Если браузер не открылся автоматически, перейдите по ссылке:")
	fmt.Println(url)
	fmt.Println("Ниже в этом окне будет журнал всех изменений лимитов IOPS (кто, что, когда).")
	fmt.Println()

	go openBrowser(url)

	if err := http.Serve(listener, guarded); err != nil {
		log.Fatal(err)
	}
}

// hostGuard отклоняет запросы с заголовком Host, отличным от 127.0.0.1/localhost.
//
// hostGuard rejects requests whose Host header isn't 127.0.0.1/localhost.
func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
