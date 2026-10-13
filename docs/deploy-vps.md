# Deploy di VPS

Opsora jalan sebagai satu Compose stack di mesin Linux. Worker memakai soket Docker host, jadi mesin ini setara dengan akses root. Pakai VPS yang khusus untuk CI, dan hanya bangun repositori yang Anda percaya.

## Kebutuhan minimum

Host hanya butuh Docker. Go tidak dipasang di VPS; image dibangun di dalam Docker.

| | Tanpa analisis AI | Dengan Ollama `phi3` |
| --- | --- | --- |
| CPU | 2 vCPU | 4 vCPU |
| RAM | 4 GB | 8 GB |
| Disk | 40 GB | 40 GB |
| Sistem | Linux 64-bit | Linux 64-bit |
| Docker | Engine 24 atau lebih baru, plugin Compose v2 | sama |
| Jaringan | IP publik atau domain, HTTPS | sama |

Angka itu cukup untuk satu build pada satu waktu.

- Satu langkah pipeline dibatasi 2 GB RAM dan 2 CPU. Container deploy dibatasi 1 GB dan 1 CPU.
- `phi3` memegang sekitar 4 GB saat dimuat. `AI_KEEP_ALIVE` bawaan `2m` melepas model dua menit setelah analisis, jadi model dan langkah pipeline biasanya tidak tinggal bersama.
- 8 GB tetap sesak kalau image deploy sedang dibangun sementara model masih di memori. Untuk itu naikkan ke 16 GB.
- Disk 40 GB mencakup Postgres, image langkah, dan Prometheus (retensi 15 hari). Naikkan kalau image bahasa atau image deploy besar.
- Tanpa AI, kosongkan `AI_SERVICE_URL` di `.env`. Layanan `ai-service` boleh tetap jalan; analisis tidak dipanggil.

## Yang harus bisa dijangkau dari internet

| Lalu lintas | Tujuan |
| --- | --- |
| Browser | `https://<domain>/` konsol |
| GitHub webhook | `https://<domain>/api/webhook/github` |
| VPS ke internet | clone repositori, tarik image, dan API Telegram kalau dipakai |

Compose sudah mengikat Postgres, Redis, Prometheus, dan Grafana ke `127.0.0.1`. Jangan mengubah itu menjadi `0.0.0.0`. Port metrik `9101`–`9104` tidak dipublikasikan. `GET /api/builds` menolak permintaan tanpa sesi konsol atau `API_READ_TOKEN`.

Docker menyisipkan aturan iptables sendiri. Firewall seperti UFW tidak menutup port yang Compose terbitkan ke `0.0.0.0`. API tetap di `0.0.0.0` supaya tunnel atau Caddy di host yang sama bisa meneruskan webhook. Untuk mengikat API ke localhost juga, timpa port-nya:

Buat `compose.vps.yml` di sebelah `docker-compose.yml`:

```yaml
services:
  api:
    ports: !override
      - "127.0.0.1:8080:8080"
```

`!override` butuh Compose v2.24 atau lebih baru. Grafana dan Prometheus tetap bisa dibuka lewat tunnel SSH, bukan dari internet.

## Pasang Docker

Di Debian atau Ubuntu:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
```

Keluar dan masuk lagi supaya grup `docker` terbaca, lalu `docker compose version`.

## Isi konfigurasi

```sh
git clone https://github.com/bionte4/opsora.git
cd opsora
cp .env.example .env
```

Wajib diganti sebelum `up`:

| Variabel | Isi |
| --- | --- |
| `GITHUB_WEBHOOK_SECRET` | String acak. Rahasia yang sama di webhook GitHub. |
| `GRAFANA_ADMIN_PASSWORD` | Bukan `change-me`. |
| `ADMIN_EMAIL` | Email admin konsol. |
| `ADMIN_PASSWORD` | Bukan `change-me`. Hanya dipakai saat tabel pengguna masih kosong. |
| `POSTGRES_PASSWORD` | Bukan `opsora`. |
| `REDIS_PASSWORD` | Bukan `opsora`. |
| `OPSORA_PUBLIC_URL` | `https://<domain>`, tanpa garis miring di akhir. |

`DATABASE_URL` dan `REDIS_URL` di `.env` dipakai kalau layanan dijalankan di host. Di dalam Compose, URL mengarah ke nama layanan `postgres` dan `redis`. Tetap samakan password di `DATABASE_URL` dan `REDIS_URL` dengan `POSTGRES_PASSWORD` dan `REDIS_PASSWORD` supaya tidak ada dua password.

Untuk repositori privat dan status commit, isi `GITHUB_TOKEN` dengan akses baca konten dan tulis status commit.

Hasilkan rahasia:

```sh
openssl rand -hex 32   # GITHUB_WEBHOOK_SECRET
openssl rand -hex 24   # CHATOPS_TOKEN, kalau perintah Telegram dipakai
```

## Jalankan

```sh
docker compose -f docker-compose.yml -f compose.vps.yml up -d --build
curl -s http://127.0.0.1:8080/health
```

Jawaban yang diharapkan: `{"status":"ok","database":"ok","redis":"ok"}`.

Admin konsol dibuat pada start pertama. Start berikutnya tidak mengatur ulang akun itu.

## HTTPS

GitHub menolak webhook tanpa HTTPS. Contoh Caddy, yang mengambil sertifikat sendiri:

```
ci.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Lalu:

```sh
docker run -d --name caddy --restart unless-stopped \
  -p 80:80 -p 443:443 \
  -v $PWD/Caddyfile:/etc/caddy/Caddyfile \
  -v caddy_data:/data \
  caddy:2
```

Buka `https://ci.example.com`, masuk dengan `ADMIN_EMAIL` dan `ADMIN_PASSWORD`.

Di GitHub, webhook repositori:

- Payload URL: `https://ci.example.com/api/webhook/github`
- Content type: `application/json`
- Secret: nilai `GITHUB_WEBHOOK_SECRET`
- Events: pushes dan pull requests

## Analisis AI di VPS

Ollama dipasang di host, bukan di Compose. Docker di Linux bisa memakai CPU; GPU tidak wajib. Untuk model gratis di internet, set `AI_PROVIDER=openai`, `AI_BASE_URL`, dan `AI_API_KEY` seperti di `.env.example`. VPS lalu cukup mengikuti kolom tanpa analisis di tabel di atas, karena model tidak dimuat di mesin ini.

```sh
curl -fsSL https://ollama.com/install.sh | sh
ollama pull phi3
```

`ai-service` sudah mengarah ke `http://host.docker.internal:11434`. Biarkan `AI_KEEP_ALIVE=2m` pada mesin 8 GB. Untuk mematikan analisis, set `AI_SERVICE_URL=` (kosong) lalu `docker compose up -d api notifier`.

## Telegram

Isi `TELEGRAM_BOT_TOKEN` dan `TELEGRAM_CHAT_ID`, lalu `docker compose up -d notifier`. Tanpa token, notifier diam.

Kalau perintah `/status`, `/logs`, `/rerun`, dan `/rollback` dipakai, isi `CHATOPS_TOKEN` yang sama untuk API dan notifier, serta `CHATOPS_ALLOWED_CHATS` dengan ID chat yang diizinkan.

## Perbarui

```sh
git pull
docker compose -f docker-compose.yml -f compose.vps.yml up -d --build
```

Volume Postgres, Redis, Prometheus, dan Grafana tidak dihapus oleh perintah itu.

Cadangkan basis data sebelum perbarui:

```sh
docker compose exec postgres pg_dump -U opsora opsora > opsora.sql
```

Sesuaikan `-U` dan nama basis data kalau `POSTGRES_USER` atau `POSTGRES_DB` diubah.
