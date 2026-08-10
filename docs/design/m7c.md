# M7C — Ciclo de vida del daemon y servicio de usuario

**Estado:** implementado; pendiente de integración protegida.
**Alcance:** pasos 50–51 de `docs/roadmap/POST_M6.md`.

## 1. Objetivo

Una persona debe poder arrancar, inspeccionar, detener y reiniciar corral sin
leer PID files ni invocar `kill`. También debe poder instalarlo como servicio
de su propia sesión:

```text
corral daemon [start]
corral daemon status [--json]
corral daemon stop [--grace D] [--timeout D]
corral daemon restart [--grace D] [--timeout D]

corral service install
corral service status [--json]
corral service uninstall
```

`corral daemon` conserva compatibilidad y equivale a `daemon start`.

## 2. Autoridades y modelo de estado

El `flock` de `<state_dir>/daemon.lock` sigue siendo la única autoridad de
exclusión. El socket/API demuestra que el daemon está atendiendo. El PID file
es sólo diagnóstico: ninguna operación le envía señales al número leído allí.

Mientras mantiene el lock, el daemon escribe atómicamente
`<state_dir>/daemon.state` con uno de:

- `starting`: lock tomado, startup aún no expuesto;
- `running`: API lista y PID file escrito;
- `stopping`: shutdown aceptado y drenaje en curso.

El archivo se elimina al cerrar normalmente. Si queda después de un crash sólo
es confiable cuando el lock continúa ocupado; con lock libre es evidencia
obsoleta.

El probe local produce estos estados:

| Estado | Evidencia |
|---|---|
| `running` | `GET /v1/version` respondió. |
| `starting` | lock ocupado + marcador `starting`. |
| `stopping` | lock ocupado + marcador `stopping`. |
| `lock-held` | lock ocupado, API no responde y marcador no es concluyente. |
| `stale-pid` | lock libre y existe `daemon.pid`. |
| `stale-files` | lock libre, sin PID, pero socket/marcador permanece. |
| `stopped` | lock libre y no quedan artefactos operativos. |

`status` retorna éxito para todo estado reconocido y error sólo si no puede
cargar configuración o inspeccionar el lock/filesystem. La salida JSON es
estable y contiene `state`, `pid`, `socket`, `state_dir`, `version`,
`api_version`, `started_at` y `uptime_ms`; campos desconocidos quedan vacíos.

## 3. Semántica de comandos

### Start

- si está `running`, termina 0 y muestra el PID existente;
- si está `starting`, espera hasta ready dentro de su timeout;
- si está `stopping`, espera a que libere el lock y arranca una vez;
- si hay PID/socket/marcador obsoleto con lock libre, el startup existente los
  reconcilia de manera segura;
- ante `lock-held` no intenta tomar control ni señalar procesos.

### Stop

- con API viva envía `POST /v1/daemon/shutdown` y espera lock libre;
- si ya está `stopping`, sólo espera;
- `stopped`, `stale-pid` y `stale-files` son éxito idempotente;
- `starting` espera ready y luego solicita shutdown;
- `lock-held` falla con diagnóstico; nunca hace fallback a `kill(pid)`;
- `--timeout` limita toda la operación y nunca escala a SIGKILL.

### Restart

Es `stop` completo seguido de `start`. El nuevo PID debe diferir del anterior.
Sólo empieza después de observar lock libre, de modo que no hay dos daemons
solapados. Las sesiones `running` se checkpointan durante stop y recovery las
reanuda según el contrato existente. Si existe un descriptor de servicio, el
start final se hace mediante launchd/systemd; nunca se sustituye sin aviso un
daemon supervisado por otro detached.

## 4. Servicio macOS (launchd)

Archivo: `~/Library/LaunchAgents/com.djbu.corral.plist`.

- dominio: `gui/<uid>`;
- `ProgramArguments`: ruta absoluta del binario, `daemon`, `--foreground`;
- `RunAtLoad=true`;
- `KeepAlive.SuccessfulExit=false`: reinicia tras crash, no tras stop limpio;
- `ProcessType=Background`;
- working directory y logs con rutas absolutas;
- `launchctl bootstrap`, `print`, `kickstart` y `bootout`; no APIs obsoletas `load/unload`;
- no `EnvironmentVariables`, tokens ni contenido de config.

Una definición activa sin cambios no se reescribe ni reinicia. Una definición
instalada pero inactiva usa `kickstart`. Si cambia, se hace bootout ordenado,
reemplazo atómico y bootstrap. `uninstall` hace bootout y elimina sólo el
plist; nunca `~/.corral`.

## 5. Servicio Linux (systemd --user)

Archivo: `~/.config/systemd/user/corral.service`.

- `Type=simple`;
- `ExecStart=<binario absoluto> daemon --foreground` con escaping systemd;
- `Restart=on-failure` y `RestartSec=2`;
- `WorkingDirectory=<home absoluto>`;
- `UMask=0077`;
- `WantedBy=default.target`;
- operación exclusiva mediante `systemctl --user`.

Install escribe atómicamente, ejecuta `daemon-reload` y `enable --now`.
Una definición cambiada reinicia el unit sólo si ya estaba activo. Uninstall
ejecuta `disable --now`, elimina sólo el unit y vuelve a hacer daemon-reload.

## 6. Seguridad

| Riesgo | Control |
|---|---|
| PID reciclado | Nunca señalar por PID file; API + lock son autoridad. |
| Dos daemons durante restart | Esperar liberación real del flock. |
| Lock vivo sin API | Fallar cerrado como `lock-held`. |
| Symlink/archivo arbitrario como unit | Directorios del usuario, target exacto, archivo regular y rename atómico. |
| Ejecución como root | `service` rechaza UID 0. |
| Inyección en plist/unit | XML escaping y quoting systemd dedicado. |
| Filtración de secretos | No copiar entorno ni config al descriptor. |
| Uninstall destructivo | Sólo descriptor; datos y binario permanecen. |
| Manager ausente | Error explícito, sin fallback shell improvisado. |

La ruta del binario se captura de `os.Executable()` y su SHA-256 se incluye como
comentario no secreto del descriptor. Un upgrade que reemplaza atómicamente el
mismo path cambia la definición determinísticamente y provoca su restart sin
copiar el binario ni perder la definición del servicio.

## 7. Pruebas

### Deterministas

- tabla completa del probe lifecycle;
- marker atómico y permisos `0600`;
- start/stop/restart repetidos;
- timeout y `lock-held` no envían señales;
- salida humana/JSON;
- render determinista de plist y unit para rutas con espacios/caracteres especiales;
- install/status/uninstall idempotente con runner falso;
- cambio de definición, crash/restart y login/logout simulados;
- root y OS no soportado rechazados;
- uninstall conserva state dir y binario.

### E2E

- binario real: start→status→start→stop→stop;
- restart cambia PID, recupera una sesión fake activa y no necesita SIGKILL;
- shutdown con tarea headless activa termina ordenadamente;
- gates normales y `go test -race ./...` en macOS/Linux;
- smoke opt-in de manager real en una cuenta/VM desechable. El suite normal no
  modifica launchd/systemd del desarrollador ni depende de login interactivo.

## 8. Cierre M7C

- comandos daemon cumplen la tabla e idempotencia;
- restart conserva sesiones recuperables;
- descriptores launchd/systemd deterministas, sin secretos y con rutas absolutas;
- pruebas fake demuestran restart-on-crash y enable-on-login;
- uninstall conserva datos;
- smoke real por plataforma documentado o, si el host CI no expone un user
  manager, gate opt-in documentado con la limitación explícita;
- manuales, roadmap y evidencia actualizados;
- PR protegido integrado y CI posterior al merge verde.
