# Evidencia M7E — operación segura de datos

**Fecha:** 2026-08-10 (America/Bogota)
**Alcance:** paso 53 de M7

## Evidencia funcional

- un snapshot tomado con el store abierto preservó sesiones, eventos, DAGs,
  presupuestos, token revocado, learning y su `learning_evidence`;
- restore materializó una DB standalone, conservó los conteos y rechazó un
  `user_version=999` con `ErrSchemaTooNew`;
- la CLI creó backup caliente, mutó el original, restauró el snapshot y dejó un
  backup automático del estado sustituido;
- GC dry-run no mutó; apply eliminó sólo un directorio huérfano y conservó el
  directorio cuyo ID seguía en SQLite;
- symlinks candidatos fallaron cerrados;
- WAL terminó truncado y `VACUUM` sólo corrió al alcanzar el umbral;
- una guardia de espacio simulada rechazó spawn antes de crear el directorio
  de sesión.

## Gates

```sh
go test ./internal/store ./internal/dataops ./internal/config \
  ./internal/supervisor ./cmd/corral
go test -race ./...
CGO_ENABLED=0 go build ./...
go vet ./...
git diff --check
```

Restore y GC se probaron bajo el flock de mantenimiento. No se ejecutó GC
contra `~/.corral` real: sus pruebas usan state dirs desechables y verifican
exactamente qué rutas sobreviven.
