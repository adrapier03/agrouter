# agrouter — High-Performance Antigravity / Gemini Code Assist Router

`agrouter` adalah router mandiri berkecepatan tinggi berbasis **Go stdlib** (single binary, footprint RAM sangat ringan ~15-25 MB) khusus untuk mengelola pool akun **Google Antigravity / Gemini Code Assist** dengan endpoint standar **OpenAI-compatible (`/v1`)**.

Didesain khusus untuk menggantikan solusi berbasis web framework berat (seperti Next.js yang memakan 400+ MB RAM) pada host server terbatas (ARM64 / VPS / Android chroot), serta dilengkapi dengan otomatisasi onboarding akun Google Workspace (GSuite), pelacak kuota, analitik token (Usage), dan integrasi tool-calling penuh untuk agen AI otonom (Hermes, Cline, Claude Code).

---

## Fitur Utama

- **OpenAI-Compatible (`/v1/chat/completions`, `/v1/models`):**
  - Streaming SSE (`stream: true`) dengan translasi chunk real-time.
  - Non-streaming completion (`stream: false`).
  - System prompt & multi-turn message translation ke Google v1internal payload format.
- **Full Tool Calling Parity untuk AI Agents:**
  - Mendukung function calling (`tools`, `tool_choice`, `role: tool`).
  - **Thought Signature Sentinel Whitelist:** Menyuntikkan sentinel signature pada percakapan multi-turn yang memanggil tool agar tidak ditolak oleh Google API (`400 INVALID_ARGUMENT: missing thought_signature`).
  - **Schema Sanitizer:** Membersihkan format schema enum integer/number (seperti pada tool Discord/MCP) ke format string agar kompatibel dengan protobuf deserializer Google.
  - **Auto Tool Name Resolution:** Memetakan `tool_call_id` secara otomatis ke nama tool asli pada saat replay riwayat percakapan.
- **Katalog Model Lengkap:**
  - `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-low`
  - `gemini-3.7-flash-high`, `gemini-3.7-flash-medium`, `gemini-3.7-flash-low`
  - `gemini-3.6-flash-high`, `gemini-3.6-flash-medium`, `gemini-3.6-flash-low`
  - `gemini-3.5-flash-low`, `gemini-3.5-flash-extra-low`
  - `claude-sonnet-4-6`, `claude-opus-4-6-thinking`
  - `gpt-oss-120b-medium`, `gemini-3-flash-agent`, `gemini-pro-agent`
- **Multi-Account Pool & Auto Refresh:**
  - Dynamic round-robin load balancing antar akun aktif.
  - Failover otomatis: jika satu akun limit (429) atau overload (503/529), router otomatis memutar ke akun berikutnya.
  - Refresh token on-demand (lead 90 detik) tanpa intervensi manual.
- **Otomatisasi Onboarding GSuite (`ag_gsuite`):**
  - Worker headless berbasis Node.js + [CloakBrowser](https://cloakbrowser.dev) untuk login Google Workspace, menyetujui izin OAuth Antigravity, mengambil refreshToken & `projectId`, dan langsung mendaftarkan akun ke pool `agrouter`.
  - Dijalankan langsung dari dashboard admin via tombol `+ AKUN` atau via CLI.
- **Dashboard Admin Monospace Gelap (`/admin`):**
  - **Batch Actions:** Checkbox select-all, aktifkan massal, matikan massal, dan hapus akun terpilih.
  - **Quota Monitor:** Menampilkan persentase sisa kuota dan waktu reset per-model untuk setiap akun.
  - **Usage & Token Analytics:** Pelacak request, input token (prompt), output token (completion), cached token, dan total token untuk rentang waktu **Today, 1 Hari, 7 Hari, 30 Hari, dan 60 Hari**, lengkap dengan tabel rincian per-model dan riwayat harian.
  - **API Key Gate:** Manajemen token akses `/v1` (`agk-...`) dengan counter penggunaan.
  - **Live Console:** Stream log server real-time (SSE dengan fallback auto-polling 3s).

---

## Struktur Repositori

```text
agrouter/
├── main.go            # HTTP router, wire protocol adapter, handler SSE & Admin
├── store.go           # Account store, JSON persistence, OAuth token refresher
├── usage.go           # Token tracker, analytics engine, timeframe aggregator
├── dashboard.html     # Single-page UI (embedded ke Go binary via //go:embed)
├── go.mod             # Go module definition
├── ag_gsuite/         # Automasi onboarding akun GSuite
│   ├── ag_gsuite.js   # Script automation CloakBrowser + PKCE loopback listener
│   └── package.json   # Dependensi npm (cloakbrowser)
├── .gitignore
└── README.md
```

---

## Persyaratan Sistem

- **Go 1.20+** (untuk build binary `agrouter`).
- **Node.js 18+** (hanya jika menggunakan modul otomatisasi onboarding GSuite).

---

## Panduan Instalasi & Menjalankan

### 1. Build Binary

```bash
git clone https://github.com/adrapier03/agrouter.git
cd agrouter

# Kompilasi Go binary
go build -o agrouter .
```

### 2. Konfigurasi Environment (Opsional)

`agrouter` bekerja dengan konfigurasi default yang siap pakai, namun dapat disesuaikan melalui environment variables:

| Variabel | Default | Keterangan |
|---|---|---|
| `AGROUTER_LISTEN` | `127.0.0.1:20129` | Alamat host & port agrouter |
| `AGROUTER_DATA` | `/root/agrouter-data/accounts.json` | Path penyimpanan data akun & API keys |
| `AGROUTER_USAGE` | `/root/agrouter-data/usage.jsonl` | Path log transaksi token analytics |
| `AGROUTER_LOG` | `/var/log/apps/agrouter.log` | Path log sistem |
| `AGR_ADMIN_TOKEN` | `agrouter-admin` | Token autentikasi panel dashboard `/admin` |
| `AG_GSUITE_DIR` | `ag_gsuite` | Direktori modul otomatisasi GSuite |

### 3. Setup Modul GSuite Onboard (Opsional)

Jika ingin menggunakan fitur auto-add akun GSuite:

```bash
cd ag_gsuite
npm install
cd ..
```

### 4. Menjalankan Service

```bash
./agrouter
```

Buka browser ke `http://127.0.0.1:20129/admin` dan masukkan admin token Anda.

---

## Menambahkan Akun GSuite Otomatis

1. Buka dashboard `/admin`.
2. Klik tombol **`+ AKUN`**.
3. Pada tab **`⚡ Auto Onboard GSuite`**, masukkan daftar akun dengan format:
   ```text
   email1@domain.com|password123
   email2@domain.com|password123
   ```
4. Klik **`▶ PROSES ONBOARD GSUITE`**.
5. Proses akan berjalan di background server secara headless, dan progresnya dapat dipantau langsung pada jendela **Console** di dashboard.

---

## Konfigurasi Reverse Proxy Nginx (Domain Publik)

Untuk menghubungkan `agrouter` ke domain publik dengan SSL (misal `https://ad.example.com`):

```nginx
server {
    listen 80;
    server_name ad.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name ad.example.com;

    ssl_certificate /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    location /admin {
        proxy_pass http://127.0.0.1:20129;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    location / {
        proxy_pass http://127.0.0.1:20129;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 600s;
    }
}
```

---

## Lisensi

MIT License. Dibuat dan dioptimalkan untuk efisiensi server mandiri.
