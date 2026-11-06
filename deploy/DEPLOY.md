# Putting VaultDB online (Oracle Cloud free tier + a Vercel domain)

About 30 minutes, once. After that, updating is one command.

## 1. Create the server (Oracle Cloud, free forever)

1. Sign up at [cloud.oracle.com](https://cloud.oracle.com) (choose your home region carefully: free servers stay in that region). Oracle asks for a card to verify you; the Always Free resources are not charged.
2. Open **Compute → Instances → Create instance**.
   - **Image:** Ubuntu 24.04 (or 22.04).
   - **Shape:** *Ampere* `VM.Standard.A1.Flex` with 1 OCPU and 6 GB memory (Always Free). If Ampere is "out of capacity", use `VM.Standard.E2.1.Micro` instead, which is also free.
   - **SSH keys:** choose *Generate a key pair* and download the private key. Keep it safe.
3. When it is running, copy its **Public IP address**.
4. Open the server's firewall in Oracle: on the instance page, click the **Subnet** link → the **Default Security List** → **Add Ingress Rules**, twice:
   - Source CIDR `0.0.0.0/0`, IP protocol TCP, destination port `80`
   - Source CIDR `0.0.0.0/0`, IP protocol TCP, destination port `443`

## 2. Point your domain at it (Vercel)

1. In Vercel, open **Domains** and buy the domain you want (or use one you already have there).
2. Open the domain's **DNS records** and add:

   | Type | Name | Value |
   |---|---|---|
   | A | `@` (leave empty) | your server's public IP |
   | A | `www` | your server's public IP |

3. Wait a few minutes. You can check with `nslookup yourdomain.com`: it should show the server's IP.

The website runs on the Oracle server; Vercel only provides the name. That's needed because VaultDB keeps visitors' sandboxes in a long-running program, which Vercel's serverless hosting can't do.

## 3. Install VaultDB on the server

From Windows, open **PowerShell** and connect (replace the key file name and IP):

```powershell
ssh -i $HOME\Downloads\ssh-key.key ubuntu@YOUR.SERVER.IP
```

If Windows complains that the key's permissions are too open, run this once and try again:

```powershell
icacls $HOME\Downloads\ssh-key.key /inheritance:r /grant:r "$($env:USERNAME):(R)"
```

Then, on the server, run (with your domain):

```bash
curl -fsSL https://raw.githubusercontent.com/ABDULRAIYAAN/vaultdb/main/deploy/oracle-setup.sh | sudo DOMAIN=yourdomain.com bash
```

It installs everything, starts the site, and Caddy fetches a free HTTPS certificate. Open `https://yourdomain.com`.

## Day to day

| Task | Command (on the server) |
|---|---|
| Update to the latest code | run the same `curl ... | sudo DOMAIN=... bash` line again |
| See the logs | `sudo journalctl -u vaultdb-web -f` |
| Turn on AI answers in "Ask the docs" | `sudo nano /etc/vaultdb/env`, set e.g. `VAULTDB_LLM=gemini` and `GEMINI_API_KEY=...`, then `sudo systemctl restart vaultdb-web` |
| Restart | `sudo systemctl restart vaultdb-web` |

## What the setup does for safety

- The site runs as a throwaway system user that cannot write anywhere, see home folders, gain privileges or reach devices (systemd hardening in `vaultdb-web.service`), capped at 600 MB of memory.
- It listens only on `127.0.0.1`; the internet reaches it through Caddy, which handles HTTPS and certificate renewal.
- API keys live in `/etc/vaultdb/env`, readable only by root and the service, never in the code.
- Caddy's access logs are switched off, so visitor IP addresses are not stored (matching the privacy page).
