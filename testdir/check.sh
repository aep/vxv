echo "== whoami/pwd =="; id -u; pwd
echo "== write to PWD (should succeed) =="; (echo hello > ./from_guest.txt && echo "WROTE ok: $(cat ./from_guest.txt)") || echo "WRITE FAILED"
echo "== write to / (should FAIL) =="; (touch /guest_should_fail 2>/dev/null && echo "UNEXPECTED: wrote /") || echo "root read-only: OK"
echo "== root mount ro? =="; grep -E ' / ' /proc/mounts | head -1
echo "== pwd mount =="; grep -E "$(pwd)" /proc/mounts | head -1
echo "== tmpfs =="; grep -E ' /tmp ' /proc/mounts | head -1
echo "== TSI internet =="; (getent hosts example.com >/dev/null 2>&1 && echo "DNS resolves") || echo "no getent"; \
  if command -v curl >/dev/null; then curl -sS -m 10 -o /dev/null -w "HTTP %{http_code}\n" https://example.com || echo "curl failed"; \
  elif command -v wget >/dev/null; then wget -qO- -T 10 https://example.com >/dev/null && echo "wget ok" || echo "wget failed"; \
  else echo "no curl/wget"; fi
