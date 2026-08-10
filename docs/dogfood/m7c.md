# Evidencia M7C — lifecycle y servicio de usuario

**Fecha:** 2026-08-10 (America/Bogota)
**PR/SHA:** se completan al integrar la rama M7C.

## Alcance validado

- `corral daemon start|status|stop|restart`, incluida la compatibilidad de
  `corral daemon` como start;
- estados `running`, `starting`, `stopping`, `lock-held`, `stale-pid`,
  `stale-files` y `stopped` basados en API + flock;
- PID file exclusivamente diagnóstico: stop/restart nunca envían señales al
  PID leído allí;
- `corral service install|status|uninstall` para launchd y systemd de usuario;
- descriptores deterministas, rutas absolutas, XML/quoting seguro y ausencia
  de campos de entorno/secretos;
- huella SHA-256 del ejecutable para detectar upgrades aun sin cambiar la ruta;
- install, upgrade, reactivación y uninstall idempotentes;
- start/restart conservan la propiedad del manager cuando hay servicio instalado;
- rechazo de root, OS no soportado, rutas relativas, control characters y
  descriptor symlink/no regular;
- uninstall conserva la base, estado, logs y binario.

## Pruebas funcionales

La E2E del binario real ejecuta:

1. start → segundo start sin duplicar daemon → status JSON;
2. restart ordenado y comprobación de PID nuevo;
3. stop → segundo stop idempotente;
4. PID obsoleto y lock vivo sin API, comprobando fallo cerrado;
5. sesión fake interactiva activa con transcript durable;
6. restart ordenado, recuperación con `--resume` y `resume_count=1`;
7. crash acotado del PID obtenido de la API, nuevo startup y
   `resume_count=2` con proceso estable;
8. stop final sin SIGKILL.

Una prueba separada checkpointa un proceso headless activo sin PTY y confirma
estado durable `exited` con intención `running`. Las pruebas del service manager
simulan launchd/systemd con un runner con estado: enable al login, política de
restart tras crash, reactivación, cambio de binario y disable/bootout.

## Manager real

La suite normal no registra jobs en la cuenta del desarrollador. Un smoke real
es deliberadamente opt-in y debe hacerse en una cuenta o VM desechable con el
binario instalado en una ruta estable:

```sh
corral daemon stop
corral service install
corral service status
# cerrar/abrir la sesión de usuario y volver a comprobar status
# terminar sólo el PID confirmado por `corral daemon status --json` para el smoke de crash
corral service status
corral service uninstall
test -f "$HOME/.corral/corral.db"   # los datos deben seguir presentes
```

No se ejecutó ese smoke destructivo sobre el launchd real de la cuenta de
desarrollo. La lógica de managers, login, crash y uninstall está cubierta de
forma aislada; CI macOS/Linux ejecuta las pruebas sin requerir una sesión
gráfica, systemd vivo, root, red ni secretos.

## Gates

Gates locales verdes sobre el árbol M7C final:

- `gofmt` y `git diff --check`;
- `CGO_ENABLED=0 go build ./...`;
- builds cruzados `linux/amd64` y `darwin/arm64` sin CGO;
- `go vet ./...`;
- `go test ./...`;
- `go test -race ./...`;
- `plutil -lint` sobre el plist renderizado en macOS.

La CI protegida macOS/Linux y el SHA/PR se añaden después de publicar la rama.
