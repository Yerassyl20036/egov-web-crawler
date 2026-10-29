# Deploying to a small VM

The public demo runs on an Oracle Cloud "Always Free" VM (1 GB RAM, 2 x86 vCPUs) with Docker. Any Linux host with Docker and the compose plugin works the same way.

The stack is two containers: Postgres and the API, which also serves the web UI. On first start the API seeds the catalog from the snapshot bundled in the image. An optional Caddy container adds HTTPS.

## First deploy

1. **Create `~/ask-data-egov/.env` on the server**, readable only by your user (`chmod 600`):

   ```bash
   POSTGRES_PASSWORD=<long random string, e.g. openssl rand -hex 24>
   # one LLM provider (see the main README, "Models")
   OPENROUTER_API_KEY=sk-or-...
   LLM_PROVIDER=openrouter
   AGENT_MODEL=openrouter/free
   # limits for a public demo
   CHAT_RATE_PER_MIN=5
   CHAT_DAILY_LIMIT=200
   ```

2. **Build and ship the image** from your machine. `deploy.sh` builds for the server's CPU, streams the image over SSH (no registry) and runs `docker compose up -d`:

   ```bash
   deploy/deploy.sh <ssh-host>                 # x86 VM (Oracle E2.1.Micro)
   deploy/deploy.sh <ssh-host> linux/arm64     # Ampere A1 / other ARM VMs
   ```

3. **Open the port.** The API listens on `HTTP_PORT` (default 8080). It must be allowed by both the cloud firewall (on Oracle Cloud: VCN → Security List → ingress rule) and the VM's own `iptables`.

The site is then at `http://<server-ip>:8080`.

## HTTPS (optional)

Caddy gets a Let's Encrypt certificate automatically. It needs **port 443 open** in the cloud firewall and on the VM, and a hostname that points at the server.

- If you don't own a domain, use `ask.<ip-with-dashes>.sslip.io`, e.g. `ask.158-180-23-143.sslip.io`.
- If you do, create an A record such as `ask.example.com` pointing at the server.

```bash
echo 'DOMAIN=ask.158-180-23-143.sslip.io' >> ~/ask-data-egov/.env
echo 'COMPOSE_PROFILES=https' >> ~/ask-data-egov/.env
cd ~/ask-data-egov && docker compose up -d
```

## Day-to-day

```bash
cd ~/ask-data-egov
docker compose ps                       # status
docker compose logs -f api              # logs (JSON, one line per request)
docker compose exec api eval            # retrieval eval against the live catalog
docker compose exec db pg_dump -U egov egov | gzip > backup.sql.gz   # backup
```

To refresh the catalog from the portal: `docker compose exec api crawler`, which takes about 8 minutes.

Redeploy after code changes: run `deploy/deploy.sh <ssh-host>` again. The database volume is kept.
