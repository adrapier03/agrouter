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
- **Native Prompt Auto-Fold Adapter (Mitigasi 429 RESOURCE_EXHAUSTED Siluman):**
  - Mengatasi bug/limitasi kritis pada backend Google CloudCode PA API di mana pengiriman system prompt yang panjang (khas agent seperti Hermes, Cline, atau OpenCode yang membawa rules, memory host, dan katalog tools tebal) memicu penolakan palsu `HTTP 429 RESOURCE_EXHAUSTED` meskipun kuota akun masih penuh.
  - Secara otomatis mendeteksi model Gemini/Gemma dengan system prompt > 800 karakter (atau > 4000 karakter pada model lain), memindahkan teks instruksi tebal tersebut ke awal pesan user pertama (*first user turn*) secara transparan, dan menyisakan marker minimal pada `systemInstruction`.
  - Agen AI di sisi client (Hermes di berbagai VPS/PC) dapat langsung menggunakan model Gemini tanpa perlu memodifikasi atau me-patch kode client.
- **Failover & Rotasi Penuh Pool Akun (Zero-Cap Retry):**
  - Saat suatu akun terkena limit kuota riil (429) atau error 404/503, `agrouter` tidak membatasi percobaan ke segelintir akun, melainkan otomatis mencoba berputar ke **seluruh akun aktif di pool** secara berurutan tanpa memutus sesi pengguna.
- **Katalog Model Lengkap:**
  - `gemini-4-neon-high`, `gemini-4-neon-medium`, `gemini-4-neon-low`, `gemini-4-neon-xhigh`
  - `gemini-4-hydrogen-high`, `gemini-4-hydrogen-medium`, `gemini-4-hydrogen-low`, `gemini-4-hydrogen-xhigh`
  - `gemini-4-argon-high`, `gemini-4-argon-medium`, `gemini-4-argon-low`
  - `claude-opus-5-5-high`, `claude-opus-5-5-medium`, `claude-opus-5-5-low`
  - `claude-sonnet-5-5-high`, `claude-sonnet-5-5-medium`, `claude-sonnet-5-5-low`
  - `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-low`
  - `gpt-oss-120b-medium`
  - Orchestrator is an Antigravity-native planner/executor workflow; it is not exposed as a fake single upstream model. Use the standalone Neon/Argon planner and Hydrogen executor IDs above, or define an explicit combo for fallback behavior.
- **Multi-Account Pool & Auto Refresh:**
  - Dynamic round-robin load balancing antar akun aktif.
  - Failover otomatis: jika satu akun limit (429) atau overload (503/529), router otomatis memutar ke akun berikutnya.
  - Refresh token on-demand (lead 90 detik) tanpa intervensi manual.
- **Model Combos & Virtual Routing (`🔀 COMBOS`):**
  - Menggabungkan beberapa model ke dalam satu nama virtual model (misal `agrouter` atau `gemini-claude-fallback`).
  - **Strategi Fallback (Try in order):** Prioritas model pertama (misal Gemini Flash yang kencang & hemat), otomatis beralih mulus ke model cadangan (misal Claude Sonnet) jika kuota habis atau error.
  - **Strategi Round Robin & Random:** Rotasi beban kerja antar beberapa model pilihan secara dinamis.
  - Didaftarkan otomatis ke katalog endpoint `GET /v1/models` sehingga kompatibel langsung dengan Hermes, Cursor, Cline, dan klien OpenAI apa pun.
- **Otomatisasi Onboarding GSuite (`ag_gsuite`):**
  - Worker headless berbasis Node.js + [CloakBrowser](https://cloakbrowser.dev) untuk login Google Workspace, menyetujui izin OAuth Antigravity, mengambil refreshToken & `projectId`, dan langsung mendaftarkan akun ke pool `agrouter`.
  - Dijalankan langsung dari dashboard admin via tombol `+ AKUN` atau via CLI.
- **Dashboard Admin Monospace Gelap (`/admin`):**
  - **Auto-Prune Akun Habis (Switch ON/OFF & Multi-Mode):**
    - **Mode "Habis Gemini langsung hapus":** Cocok untuk penggunaan fokus Gemini Flash. Akun langsung dihapus saat kuota Gemini mencapai 0%.
    - **Mode "Habis Gemini & Claude baru hapus":** Cocok dipadukan dengan Model Combo Fallback. Akun yang kuota Gemini-nya habis tidak langsung dihapus, melainkan tetap disimpan untuk melayani request Claude sampai kuota keduanya benar-benar 0%.
  - **One-Click Prune:** Tombol instan untuk membersihkan semua akun habis sesuai kriteria mode yang aktif.
  - **Batch Actions:** Checkbox select-all, aktifkan massal, matikan massal, dan hapus akun terpilih.
  - **Quota Monitor:** Menampilkan persentase sisa kuota mingguan dan waktu reset per-model untuk setiap akun.
  - **Recent Requests Widget:** Tabel log request per-call real-time ala 9router lengkap dengan status dot, indikator in/out token (`162.845↑ 1.430↓`), cached token badge, dan latensi.
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
| `AGROUTER_UPSTREAM_TIMEOUT` | `120s` | Batas waktu maksimal request ke Google Antigravity |
| `AGROUTER_HEADER_TIMEOUT` | `45s` | Batas waktu respon header pertama dari upstream |
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

## Mitigasi Khusus 429 & Prompt Auto-Fold (Issue #76783)

### Masalah: Error 429 Palsu pada Gemini
Pada ekosistem Google Antigravity / CloudCode PA API, server backend memiliki perilaku unik pada laci pesan sistem (`systemInstruction`):
- Aplikasi resmi **Antigravity IDE** hanya mengirimkan instruksi sistem pendek (1-2 baris).
- **AI Agent otonom (seperti Hermes, Cline, Claude Code)** menyertakan system prompt berukuran masif (puluhan ribu karakter berisi persona, aturan runtime, daftar puluhan function tools, dan memori host).
- Jika teks masif tersebut dimasukkan langsung ke field `systemInstruction` Google, backend Google sering kali menolaknya dan mengembalikan status **`HTTP 429 RESOURCE_EXHAUSTED`**, memicu ilusi seolah-olah kuota akun habis padahal kuota akun masih penuh.

### Solusi `agrouter`: Native Prompt Auto-Fold
`agrouter` mengimplementasikan adapter otomatis di level gateway:
1. **Pendeteksian Cerdas:** Setiap request model Gemini/Gemma dengan system prompt > 800 karakter (atau model non-Gemini > 4000 karakter) akan otomatis dideteksi.
2. **Auto-Folding:** Full system prompt tebal dipindahkan secara transparan ke awal pesan pengguna pertama (*first user turn*) dengan blok penanda instruksi yang terisolasi.
3. **Marker Ringkas:** Field `systemInstruction` ke upstream Google disederhanakan menjadi penanda pengarah ringkas:
   ```text
   You are an AI assistant. Follow the runtime instructions in the first user message.
   ```
4. **Zero-Patch pada Client:** Agen AI di sisi client (laptop, VPS, Poco, atau server lain) dapat menggunakan kode upstream asli tanpa perlu melakukan modifikasi atau patching manual.

### Kebijakan Retry Penuh (Full Pool Traversal)
Untuk error 429 kuota riil (TPM/RPM habis) atau error 404/503 upstream, `agrouter` menerapkan kebijakan **mencoba seluruh akun di pool** (`maxAttempts`) secara berurutan tanpa pembatasan buatan. Untuk permintaan Argon eksplisit, setelah seluruh pool dicoba, router mengembalikan error asli dan **tidak diam-diam mengganti Argon dengan Gemini Flash**. Fallback hanya berjalan jika memang dideklarasikan oleh model combo yang dipilih pengguna.

---

## Lisensi

MIT License. Dibuat dan dioptimalkan untuk efisiensi server mandiri.
