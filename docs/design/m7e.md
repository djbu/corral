# M7E — Operación segura de datos

**Alcance:** paso 53 de M7. **Estado:** contrato congelado antes del código.

## 1. Fronteras operativas

- `corral backup` funciona con el daemon vivo. SQLite produce el snapshot con
  `VACUUM INTO` desde una conexión de sólo lectura y el archivo aparece en el
  destino únicamente después de integridad, `fsync` y publicación atómica.
- `corral restore` exige daemon detenido. Valida integridad y versión, crea un
  backup automático de rollback del estado actual y sólo entonces reemplaza
  `corral.db`. Un WAL no vacío bloquea la operación; SHM no contiene datos y
  se retira al reemplazar.
- `corral gc` exige daemon detenido. El default es observar; borrar requiere
  `--apply`. Sólo considera directorios de sesión que no tienen fila en la DB,
  nunca sesiones, DAGs, tokens, eventos o evidencia de learnings referenciada.
- WAL checkpoint precede mantenimiento. `VACUUM` es opt-in y sólo corre cuando
  el freelist recuperable supera el umbral explícito/documentado.

## 2. Política de GC

Un candidato debe estar bajo `<state_dir>/sessions/<id>`, ser un directorio
real (no symlink) y no existir en `sessions.id`. La edad selecciona siempre los
candidatos que superan `--older-than`. La presión de tamaño considera sólo el
pool huérfano y añade los más antiguos hasta llevarlo bajo `--max-bytes`;
ninguna de las dos políticas vuelve elegible una sesión referenciada.

No se recorren ni borran worktrees, transcripts de Claude, backups, TLS,
configuración, logs globales ni rutas externas. Un symlink o path inesperado
falla cerrado.

## 3. Poco espacio

Todos los spawns atraviesan `supervisor.Registry.Spawn`. Antes de crear
settings, procesos o filas nuevas, una guardia consulta el filesystem de
`state_dir`. Si los bytes disponibles son menores que
`[daemon].min_free_bytes`,
el spawn falla con un error específico antes de crear settings, logs o procesos
y el daemon registra una alerta. La fila mínima de intento puede quedar como
evidencia auditable del rechazo. Sesiones ya vivas siguen operando para poder
checkpointar y liberar espacio.

El default es 256 MiB. La política pertenece al operador: archivos del repo no
pueden reducirla ni desactivarla.

## 4. Compatibilidad

Cada snapshot registra y valida `PRAGMA user_version` e `integrity_check`. Una
DB más nueva que la migración embebida más reciente falla con
`ErrSchemaTooNew`; una DB anterior se restaura y migra normalmente al próximo
arranque. Nunca se modifica `user_version` manualmente.

## 5. Gates

- writes concurrentes durante backup aparecen completos o no aparecen, nunca
  parcialmente;
- backup→restore conserva sesiones, DAGs, tokens revocados, learnings y sus
  relaciones de evidencia;
- checksum lógico/integrity y schema inválidos abortan antes de reemplazar;
- GC dry-run no muta; apply sólo elimina candidatos exactos;
- evidencia referenciada y symlinks sobreviven;
- bajo disco rechaza spawn antes de crear archivos o procesos y conserva el
  fallo como evidencia auditable;
- tests normales y race permanecen verdes en macOS/Linux.
