# Guía de administración: ctrld + NextDNS en Firewalla Gold

Actualizada: 2026-11-05 (v3.16). Copia en el router: `/data/controld/GUIA_ADMINISTRACION.md`.

## 1. Estado

- **Versión:** `v1.5.9+patched-v3.16-minmem` (ctrld oficial v1.5.9 + parches propios).
- **Upstreams:** NextDNS `d23c32` por DoH3 → Cloudflare Families → Google. Control D ya no se usa.
- **Panel:** https://my.nextdns.io/d23c32 ("Cache Boost" desactivado).

## 2. Archivos

| Qué | Ruta |
| :--- | :--- |
| Configuración | `/data/controld/ctrld.toml` (root 0600) |
| Binario | `/data/controld/ctrld` |
| Respaldos | `ctrld.bak-patched-v3.15-minmem`, `ctrld.bak-patched-v3.14-minmem` |
| Unidad systemd permanente | `/data/controld/ctrld.service` (`RestartSec=10`) |
| Script de arranque | `/home/pi/.firewalla/config/post_main.d/start_ctrld.sh` |
| Log | `/data/controld/ctrld.log` |
| Buzón con Antigravity | `/data/controld/ops/INBOX.md` (solo añadir al final) |

`/etc` vive en RAM y se borra al reiniciar: el script de arranque restaura la unidad desde `/data/controld/ctrld.service`. Cualquier cambio de la unidad se hace en esa copia.

## 3. Comandos

```bash
systemctl status ctrld
/data/controld/ctrld --version
dig @192.168.1.1 -p 5354 example.com     # directo a ctrld
dig @192.168.20.1 example.com            # como los clientes (dnsmasq)
curl -sL https://test.nextdns.io         # debe decir "protocol": "DOH3"
```

Cambiar la configuración:

```bash
sudo cp -p /data/controld/ctrld.toml /data/controld/ctrld.toml.bak.$(date +%Y%m%d_%H%M)
sudo nano /data/controld/ctrld.toml
sudo systemctl restart ctrld
```

Revertir a v3.15:

```bash
sudo cp -p /data/controld/ctrld.bak-patched-v3.15-minmem /data/controld/ctrld
sudo systemctl restart ctrld
```

## 4. Configuración clave

- Caché: `cache_size = 4096`, `cache_serve_stale = true`. **Sin** `cache_ttl_override` (alargaba TTL, contra el RFC).
- `failover_rcodes = ['SERVFAIL', 'REFUSED']`, `timeout = 1500` ms por upstream.
- Red: `192.168.1/2/10/20/30/40/50.0/24`, `10.200.152.0/24`, `10.38.182.0/24`.
- Descubrimiento: ARP y DHCP activos; mDNS y PTR inactivos.

## 5. Comportamiento

- **Failover:** si NextDNS no responde en 1,5 s o da SERVFAIL/REFUSED, pasa a Cloudflare y luego a Google. En una caída de NextDNS cada consulta sin caché espera 1,5 s.
- **Respuestas caducadas:** solo si falla el upstream, con TTL 30 s y hasta 24 h después de expirar.
- **Respaldos:** lo que responde Cloudflare o Google se cachea 60 s como máximo.
- **Filtrado:** el perfil de NextDNS aplica a toda la red (bloquea, entre otras cosas, consultas NS/DS/DNSKEY a `com`, `net`… y `*.duckdns.org`).
- **Nombres locales (sin salir a internet):** equipos conocidos con o sin `.lan`, `.local`, `.home.arpa` → su IP (TTL 60 s); nombres que no existen y `wpad`/`isatap`/`gateway`/`localhost` → NXDOMAIN local; PTR de IP privadas → nombre o NXDOMAIN.
- **Privacidad:** no se envían a los upstreams la MAC ni la subred privada. A NextDNS solo llegan nombre del equipo, IP privada y prefijo del fabricante.

## 6. Estándares (v3.16)

| Tema | v1.5.9 oficial | v3.16 | RFC |
| :--- | :--- | :--- | :--- |
| TTL de caché | TTL del primer registro | Menor TTL de la respuesta | 1035, 2181 |
| `cache_ttl_override` | Reemplaza el TTL | Solo tope (y quitado) | 1035, 2181 |
| Caché negativa | TTL del SOA | min(SOA, MINIMUM), máx. 60 s | 2308 |
| Respuestas caducadas | TTL 60 s, sin límite | TTL 30 s, máx. 24 h | 8767 |
| DNSSEC en caché | Mezcla DO/CD | Separa DO/CD | 4035, 3225 |
| Consulta a la raíz | FORMERR | Se reenvía | 1035 |
| PTR privados / `home.arpa` | Se reenvían si no se conocen | Respuesta local | 6303 / 8375 |

`.lan`, `.domain` y nombres sin punto son convención local, no RFC. No auditado: EDNS completo (6891), TCP/truncado (7766), DoH (8484), DNS64 (6147).

## 7. Parches propios

1. NDP sin bucle de CPU (issue #345) y snippet de dnsmasq en 0644 (#346); ninguno corregido en v1.5.9.
2. Descubrimiento de clientes que sobrevive a recargas; nombres desde la Redis de Firewalla.
3. Memoria: GC 50%, límite 48 MiB, caché 4096 entradas.
4. Respuestas locales, protección WPAD/ISATAP, sin MAC ni ECS privado hacia fuera.
5. Cumplimiento de estándares de la sección 6.

La versión debe llevar siempre el sufijo `+patched-...` para que ctrld no se autoactualice.

## 8. Historial (2026-11)

| Fecha | Cambio |
| :--- | :--- |
| 11-03 | v3.13: base ctrld v1.5.9 |
| 11-05 10:19 | Control D retirado; NextDNS principal |
| 11-05 19:15 | Unidad persistente + script de arranque |
| 11-05 19:33 | v3.14: sin MAC hacia fuera; `RestartSec=10` |
| 11-05 19:36 | Reinicio en frío: ctrld volvió solo en ~55 s |
| 11-05 20:01 | `cache_ttl_override` eliminado |
| 11-05 20:11 | v3.15: stale y caché negativa según RFC |
| 11-05 20:39 | v3.16: TTL mínimo, DO/CD, raíz |

## 9. Pendientes

- Observar la v3.16 unos días.
- Publicar la rama `firewalla-v3.16-minmem` en GitHub (diff en `/data/controld/patches-v3.15-to-v3.16.diff`).
