# 🔗 HookView — Local Webhook Testing Server

HookView is a lightweight, open-source tool for inspecting incoming HTTP webhook requests in real time.  
It generates temporary unique endpoints, displays live request logs through WebSockets, and provides a clean minimal interface designed for debugging and integration testing.

HookView is fully self-hosted, runs on Docker, and requires **zero external services**.

---

## ⭐ Features

- 🚀 **Generate unlimited temporary webhook endpoints**
- 🔌 **Real-time request streaming via WebSockets**
- 📡 Supports **GET, POST, PUT, DELETE and any HTTP method**
- 🌓 100% **Dark theme UI**
- 🧩 Clean minimal interface built with **TailwindCSS**
- 📜 Rich request logs:
    - Method
    - Status code
    - Headers
    - Query parameters
    - Raw body (auto-formatted when JSON)
    - Timestamp
- 🧹 Clear logs with a single click
- ✂ Delete individual logs
- 🔗 Copy webhook URL
- ♻ Stateless — logs reset on refresh
- 🐳 Full Docker support (server + WebSocket + UI)
- 💻 Built entirely in **Golang**

---

## 🏗 Tech Stack

**Backend Server**
- Go 1.22+
- net/http
- gorilla/websocket
- Custom in-memory routing
- Event broadcaster

**Frontend**
- HTML
- TailwindCSS
- Vanilla JavaScript
- WebSocket client

---

## 📦 Installation

### 1️⃣ Clone the repository

```bash
git clone https://github.com/fedejuret/hookview.git
cd hookview
```

### 2️⃣ Copy .env.example to .env
```bash
cp .env.example .env
```
You can change the default port to your favourite port.

### 3️⃣ Run it with Docker
```bash
./start_docker.sh
```


