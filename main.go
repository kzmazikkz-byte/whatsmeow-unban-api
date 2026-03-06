package main

import (
    "context"
    "encoding/json"
    "log"
    "net/http"
    "os"
    "os/signal"
    "strings"
    "syscall"
    "time"

    "go.mau.fi/whatsmeow"
    "go.mau.fi/whatsmeow/proto/waE2E"
    "go.mau.fi/whatsmeow/store/sqlstore"
    "go.mau.fi/whatsmeow/types"
    "go.mau.fi/whatsmeow/types/events"
    "google.golang.org/protobuf/proto"

    _ "github.com/mattn/go-sqlite3"
    "github.com/mdp/qrterminal"
)

var client *whatsmeow.Client
var qrChannel = make(chan string)
var authStatus = make(chan bool)

func main() {
    // Настройка логирования
    log.SetFlags(log.LstdFlags | log.Lshortfile)
    log.Println("🚀 Запуск Whatsmeow API...")

    // Инициализация базы данных с context
    dbLog := log.New(os.Stderr, "DB: ", log.LstdFlags)
    storeContainer, err := sqlstore.New(context.Background(), "sqlite3", "file:whatsapp.db?_foreign_keys=on", dbLog)
    if err != nil {
        log.Fatalf("Ошибка подключения к БД: %v", err)
    }

    // Получаем устройство с context
    device, err := storeContainer.GetFirstDevice(context.Background())
    if err != nil {
        log.Fatalf("Ошибка получения устройства: %v", err)
    }

    // Создаем клиента
    client = whatsmeow.NewClient(device, nil)

    // Обработчик событий (для QR-кода и логина)
    client.AddEventHandler(eventHandler)

    // Подключаемся к WhatsApp
    err = client.Connect()
    if err != nil {
        log.Fatalf("Ошибка подключения: %v", err)
    }

    // Запускаем HTTP сервер
    go startHTTPServer()

    // Ожидаем завершения
    c := make(chan os.Signal, 1)
    signal.Notify(c, os.Interrupt, syscall.SIGTERM)
    <-c

    client.Disconnect()
    log.Println("👋 Завершение работы")
}

// Обработчик событий WhatsApp
func eventHandler(evt interface{}) {
    switch v := evt.(type) {
    case *events.QR:
        // Пришёл QR-код для сканирования - исправлено поле Code на QRCode
        qrterminal.GenerateHalfBlock(v.QRCode, qrterminal.L, os.Stdout)
        qrChannel <- v.QRCode
    case *events.Connected:
        // Успешно подключились
        log.Println("✅ Подключено к WhatsApp!")
        authStatus <- true
    case *events.LoggedOut:
        // Разлогинились
        log.Println("❌ Разлогинен")
        authStatus <- false
    case *events.StreamReplaced:
        log.Println("🔄 Сессия заменена")
    }
}

// Запускаем HTTP сервер для команд
func startHTTPServer() {
    http.HandleFunc("/health", healthHandler)
    http.HandleFunc("/qr", qrHandler)
    http.HandleFunc("/status", statusHandler)
    http.HandleFunc("/check", checkPhoneHandler)
    http.HandleFunc("/appeal", sendAppealHandler)

    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }

    log.Printf("🌐 HTTP сервер запущен на порту %s", port)
    log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Проверка здоровья
func healthHandler(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

// Получить QR-код (в виде строки)
func qrHandler(w http.ResponseWriter, r *http.Request) {
    select {
    case qr := <-qrChannel:
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]string{
            "qr": qr,
            "message": "Отсканируй этот QR-код в WhatsApp (Настройки → Связанные устройства)",
        })
    case <-time.After(30 * time.Second):
        http.Error(w, "Таймаут ожидания QR-кода", http.StatusRequestTimeout)
    }
}

// Статус авторизации
func statusHandler(w http.ResponseWriter, r *http.Request) {
    if client == nil {
        http.Error(w, "Клиент не инициализирован", http.StatusServiceUnavailable)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "connected": client.IsConnected(),
        "logged_in": client.IsLoggedIn(),
    })
}

// Проверка номера (заблокирован или нет)
func checkPhoneHandler(w http.ResponseWriter, r *http.Request) {
    if !client.IsLoggedIn() {
        http.Error(w, "Не авторизован в WhatsApp", http.StatusUnauthorized)
        return
    }

    phone := r.URL.Query().Get("phone")
    if phone == "" {
        http.Error(w, "Параметр phone обязателен", http.StatusBadRequest)
        return
    }

    // Очищаем номер от +
    cleanPhone := strings.ReplaceAll(phone, "+", "")
    jid := types.NewJID(cleanPhone, types.DefaultUserServer)

    // Проверяем, есть ли номер в WhatsApp - исправлено на []string
    exists, err := client.IsOnWhatsApp(context.Background(), []string{cleanPhone})
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    if len(exists) == 0 || !exists[0].IsIn {
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "phone": phone,
            "exists": false,
            "status": "not_on_whatsapp",
            "message": "Номер не зарегистрирован в WhatsApp",
        })
        return
    }

    // Пробуем получить информацию о пользователе
    userInfo, err := client.GetUserInfo(context.Background(), []types.JID{jid})
    if err != nil {
        // Если ошибка, возможно номер заблокирован
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "phone": phone,
            "exists": true,
            "status": "blocked",
            "message": "Номер заблокирован или недоступен",
            "error": err.Error(),
        })
        return
    }

    // Номер активен
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "phone": phone,
        "exists": true,
        "status": "active",
        "message": "Номер активен в WhatsApp",
        "info": userInfo[jid],
    })
}

// Отправка апелляции (сообщения в поддержку)
func sendAppealHandler(w http.ResponseWriter, r *http.Request) {
    if !client.IsLoggedIn() {
        http.Error(w, "Не авторизован в WhatsApp", http.StatusUnauthorized)
        return
    }

    if r.Method != http.MethodPost {
        http.Error(w, "Метод не разрешен", http.StatusMethodNotAllowed)
        return
    }

    var req struct {
        Phone string `json:"phone"`
        Text  string `json:"text"`
    }

    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "Неверный формат JSON", http.StatusBadRequest)
        return
    }

    if req.Phone == "" || req.Text == "" {
        http.Error(w, "Поля phone и text обязательны", http.StatusBadRequest)
        return
    }

    cleanPhone := strings.ReplaceAll(req.Phone, "+", "")
    jid := types.NewJID(cleanPhone, types.DefaultUserServer)

    // Создаем сообщение
    msg := &waE2E.Message{
        Conversation: proto.String(req.Text),
    }

    // Отправляем
    resp, err := client.SendMessage(context.Background(), jid, msg)
    if err != nil {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]interface{}{
            "phone": req.Phone,
            "success": false,
            "message": "Ошибка отправки",
            "error": err.Error(),
        })
        return
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "phone": req.Phone,
        "success": true,
        "message": "Сообщение отправлено в поддержку",
        "id": resp.ID,
        "timestamp": resp.Timestamp,
    })
}
