# Guía humana para usar corral

Esta guía parte de un checkout de `v0.6.0` y cubre el flujo diario. corral está
en pre-alpha: úselo primero en repositorios con control de versiones y revise
siempre los cambios producidos por agentes.

## 1. Requisitos

- macOS o Linux;
- Go 1.26 para compilar este checkout;
- Claude Code instalado, autenticado y disponible como `claude`;
- Git para tareas con `--worktree` y para `corral review`.

Compruebe las dependencias:

```sh
go version
claude --version
git --version
```

## 2. Compilar e instalar localmente

Desde la raíz del repositorio:

```sh
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/corral" ./cmd/corral
export PATH="$HOME/.local/bin:$PATH"
corral --version
```

Un build directo muestra versión `dev`. Para incrustar la versión del tag:

```sh
go build \
  -ldflags "-X github.com/danielbecerra/corral/internal/version.Version=0.6.0 -X github.com/danielbecerra/corral/internal/version.Commit=$(git rev-parse --short HEAD)" \
  -o "$HOME/.local/bin/corral" ./cmd/corral
```

Todavía no hay instalador ni servicio launchd/systemd. El binario es la unidad
de instalación.

## 3. Diagnóstico inicial

Antes del primer daemon:

```sh
corral doctor
corral config --cwd /ruta/al/repositorio
```

`doctor` revisa hooks globales ajenos a corral y el estado conocido de Claude
Auto Mode. No modifica `~/.claude`. `config` muestra cada valor efectivo, su
fuente y cualquier clave de `.corral.toml` rechazada por seguridad.

## 4. Configuración mínima

No hace falta un archivo para empezar. Si desea uno, cree
`~/.corral/config.toml` con permisos `0600`:

```toml
[daemon]
log_level = "info"
log_format = "text"
shutdown_grace = "5s"

[session]
claude_bin = "claude"
term = "xterm-256color"
scrollback_lines = 2000
output_log_max_bytes = "8MiB"

[attach]
prefix_key = "C-\\"
detach_key = "d"

[state]
idle_timeout = "0s"

[learn]
window = "336h"
min_approvals = 3
ttl = "2160h"
min_sessions = 5
min_terminal_tasks = 3
cost_regression_tolerance = 0.10
```

`idle_timeout = "0s"` deshabilita el reaper. Para checkpointar sesiones tras
una hora de inactividad humana, use `"1h"` y reinicie el daemon.

Una `.corral.toml` dentro del proyecto sólo puede contener:

```toml
[session]
model = "sonnet"
term = "xterm-256color"
scrollback_lines = 4000
```

Ninguna configuración del repo puede cambiar permisos, secretos, red, hooks,
binario ejecutado o políticas de aprendizaje.

## 5. Arrancar y detener el daemon

Arranque separado del terminal:

```sh
corral daemon
corral ls
```

Para depuración, `corral daemon --foreground` deja logs en la terminal. En modo
normal:

```sh
tail -f "$HOME/.corral/daemon.log"
```

No existe todavía `corral daemon stop`. Para un cierre ordenado, envíe SIGTERM
al PID registrado:

```sh
kill "$(cat "$HOME/.corral/daemon.pid")"
```

No use SIGKILL salvo que el proceso no responda; evita el checkpoint ordenado.

## 6. Flujo interactivo diario

Crear una sesión en el repositorio actual y entrar inmediatamente:

```sh
corral new --cwd . --name mi-tarea
```

Dentro del attach se usa Claude Code normalmente. Para salir sin detenerlo,
presione `Ctrl-\` y después `d`. Son dos teclas consecutivas, no simultáneas.

Después:

```sh
corral ls
corral attach mi-tarea
```

Por defecto, un segundo attach toma control y desconecta al primero. Para
rechazar la toma de control:

```sh
corral attach --no-take-over mi-tarea
```

Crear sin conectar:

```sh
corral new --cwd . --name fondo --no-attach
```

Cambiar modelo sólo para esa sesión:

```sh
corral new --cwd . --name revision --model sonnet
```

## 7. Entender `corral ls`

```sh
corral ls
corral ls --json
```

Las columnas más útiles son:

- `STATE`: evidencia del agente, no texto inferido del terminal;
- `ATTACHED`: si hay una terminal cliente conectada;
- `CWD`: directorio de trabajo real;
- `PID`: proceso supervisado.

Si aparece `blocked: ...`, Claude espera una acción. `working?` o
`unknown (hooks not firing?)` indica que los hooks están atrasados o no llegan;
ejecute `corral doctor` y revise `daemon.log`.

## 8. Responder, detener y despertar

Enviar texto como si se escribiera en el PTY:

```sh
corral answer mi-tarea "sí"
corral answer mi-tarea "texto sin Enter" --no-newline
```

Enviar una tecla nombrada:

```sh
corral answer mi-tarea --key enter
corral answer mi-tarea --key esc
corral answer mi-tarea --key ctrl-c
```

No existe un comando especial “approve”: se envía exactamente el input que
Claude espera y los hooks posteriores prueban el cambio de estado.

Detener una sesión definitivamente:

```sh
corral kill mi-tarea
corral kill --grace 10s mi-tarea
```

Reanudar una sesión checkpointed o recuperable:

```sh
corral wake mi-tarea
corral attach mi-tarea
```

`kill` expresa intención durable de detener; `wake` expresa intención de
reanudar. No son equivalentes a detach.

## 9. Ejecutar una tarea headless

Para un trabajo no interactivo:

```sh
corral run --repo /ruta/al/repo \
  --name pruebas \
  --worktree \
  --budget-usd 2.00 \
  --max-attempts 2 \
  "Ejecuta las pruebas y corrige el fallo"
```

Sin `--detach`, la CLI espera y muestra cambios de estado. Con `--detach`,
imprime el ID del DAG y vuelve inmediatamente:

```sh
dag_id=$(corral run --repo "$PWD" --worktree --detach "Revisa el módulo auth")
corral review "$dag_id"
corral review --diff "$dag_id"
```

Use `--worktree` para aislar cambios de tareas automatizadas. Actualmente
`review` sólo inspecciona: aceptar, integrar o descartar la rama se hace con Git
después de revisión humana.

`--permission-mode` sólo se admite como argumento explícito en el modo de una
tarea. No puede venir de un archivo DAG ni del repositorio.

## 10. DAG de varias tareas

Cree `dag.toml`:

```toml
budget_usd = 5.0

[[node]]
name = "tests"
prompt = "Ejecuta las pruebas, identifica la causa y documenta el hallazgo"
repo = "/ruta/al/repo"
worktree = true
model = "sonnet"
max_attempts = 2
budget_usd = 2.0

[[node]]
name = "fix"
prompt = "Implementa la corrección usando el diagnóstico anterior"
repo = "/ruta/al/repo"
worktree = true
depends_on = ["tests"]
budget_usd = 2.0

[[node]]
name = "review"
prompt = "Revisa la corrección y ejecuta la suite relevante"
repo = "/ruta/al/repo"
worktree = true
depends_on = ["fix"]
budget_usd = 1.0
```

Ejecute e inspeccione:

```sh
corral run --file dag.toml
corral review
```

Los nombres deben ser únicos y cada dependencia debe existir. El parser es
estricto: rechaza claves desconocidas, incluido `permission_mode`.

## 11. Notificaciones

Ejemplo de aviso saliente con ntfy en `~/.corral/config.toml`:

```toml
[notify]
enabled = true
on = ["blocked", "exited"]
debounce = "30s"
timeout = "10s"
retries = 3

[notify.ntfy]
enabled = true
server = "https://ntfy.sh"
topic = "un-topic-largo-y-secreto"
priority = "default"
```

También existe un backend webhook con `notify.webhook.url` y headers. Los
topics, tokens y headers son secretos: mantenga el archivo en `0600` y no los
coloque en `.corral.toml`.

El canal de respuestas ntfy es una capacidad de input remoto más sensible.
Debe estar habilitado explícitamente, usar un topic diferente al de salida y
tener token. Una configuración insegura hace fallar el arranque en vez de
degradarse silenciosamente.

## 12. Acceso remoto y dashboard

El acceso remoto está apagado por defecto. En la máquina servidor:

```toml
[daemon]
listen = "0.0.0.0:8443"
```

Si no se indican `tls_cert` y `tls_key`, corral genera el material TLS
autofirmado en su estado. Para certificados administrados por usted, configure
ambas rutas; nunca una sola.

Cree un token admin localmente en el servidor:

```sh
corral token create --label laptop
corral token list
```

El secreto se muestra una sola vez. En el cliente, guárdelo en
`~/.corral/config.toml`, no en argumentos:

```toml
[client]
host = "servidor.example:8443"
token = "crl_..."
cacert = "/ruta/al/certificado-o-ca.pem"
```

Ahora los comandos compatibles pueden usar el remote resuelto o `--host`:

```sh
corral ls
corral answer sesion-remota "continúa"
corral learnings list
```

Abra `https://servidor.example:8443/` para el dashboard e introduzca el token.
La shell visual es pública, pero no obtiene datos ni ejecuta acciones sin el
bearer token.

Limitaciones actuales: `attach`, `new`, `run` y `review` son locales porque
dependen del PTY o de rutas/worktrees del filesystem del daemon. La operación
remota de sesiones existentes se hace con dashboard, `ls`, `answer`, `kill`,
`wake` y las superficies que aceptan `--host`.

Revoque un token perdido:

```sh
corral token revoke <token-id>
```

## 13. Learning loop

Después de varias aprobaciones manuales del mismo comando exacto:

```sh
corral learnings scan --repo "$PWD"
corral learnings list --repo "$PWD" --status proposed
corral learnings show <id> --diff
```

Adopte sólo después de revisar regla, evidencia y diff:

```sh
corral learnings adopt <id>
```

La regla entra en settings fijados de sesiones futuras, no modifica archivos
del repo. También puede rechazar o retirar:

```sh
corral learnings reject <id> --reason "demasiado específico"
corral learnings retire <id>
```

El reporte normal espera la ventana configurada, 14 días por defecto:

```sh
corral learnings report <id>
```

Si ya existen al menos 5 sesiones y 3 tareas terminales tanto en baseline como
en post, el operador puede cerrar ahora:

```sh
corral learnings report <id> --early --json
```

Una muestra insuficiente produce conflicto y no escribe medición. Mire siempre
los denominadores, `blocked_per_session`, `cost_per_terminal_task` y el
veredicto; `inconclusive` no significa mejora.

## 14. Copias de seguridad y actualización

La copia más segura se hace con el daemon detenido para incluir SQLite, WAL y
settings de forma coherente:

```sh
kill "$(cat "$HOME/.corral/daemon.pid")"
cp -R "$HOME/.corral" "/ruta/segura/corral-backup"
corral daemon
```

Antes de actualizar:

1. detenga el daemon ordenadamente;
2. respalde `~/.corral`;
3. compile o instale el binario nuevo;
4. arranque el daemon, que aplicará migraciones dentro del lock;
5. ejecute `corral --version`, `corral doctor`, `corral config` y `corral ls`.

No edite `corral.db` a mano ni reutilice simultáneamente el mismo `state_dir`
desde dos daemons.

## 15. Solución de problemas

### “another corral daemon holds the lock”

Ya hay una instancia usando ese `state_dir`. Revise:

```sh
cat "$HOME/.corral/daemon.pid"
ps -p "$(cat "$HOME/.corral/daemon.pid")"
tail -n 100 "$HOME/.corral/daemon.log"
```

No borre el lock mientras el proceso siga vivo.

### `unknown (hooks not firing?)`

```sh
corral doctor
corral config --cwd /ruta/al/repo
tail -n 200 "$HOME/.corral/daemon.log"
```

Compruebe que el binario `corral` usado por los hooks sigue en la misma ruta y
que el daemon fue reiniciado después de cambiar entorno o configuración.

### No puedo hacer attach remoto

Es una limitación deliberada de `v0.6.0`; attach usa un transporte local de
PTY. Use el dashboard para ver y responder remotamente.

### Una tarea no ejecuta su dependiente

El dependiente sólo se libera cuando sus requisitos terminan `succeeded`.
Revise costos, presupuesto, intentos y diff:

```sh
corral review <dag-id>
corral review --diff <dag-id>
```

### El reporte temprano no se genera

`--early` exige muestras suficientes en ambas fases. Continúe usando sesiones y
tareas reales o espere la ventana normal; el fallo no persiste un reporte
parcial.

## 16. Referencia rápida

| Necesidad | Comando |
|---|---|
| Ver versión/ayuda | `corral --version`, `corral --help` |
| Diagnóstico/config | `corral doctor`, `corral config --cwd DIR` |
| Arrancar daemon | `corral daemon` |
| Listar sesiones | `corral ls [--json]` |
| Crear/entrar | `corral new --cwd DIR [--name N]` |
| Reconectar | `corral attach NAME` |
| Responder | `corral answer NAME TEXT` |
| Detener/reanudar | `corral kill NAME`, `corral wake NAME` |
| Ejecutar tarea | `corral run --repo DIR [--worktree] PROMPT` |
| Ejecutar DAG | `corral run --file dag.toml` |
| Revisar resultados | `corral review [--diff] [DAG]` |
| Tokens remotos | `corral token create\|list\|revoke` |
| Aprendizajes | `corral learnings scan\|list\|show\|adopt\|reject\|retire\|report` |

Para entender por qué el sistema está construido de esta manera, continúe con
[Arquitectura de corral](ARQUITECTURA.md).
