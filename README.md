# Secret Copy Controller 

## Quick overview

* Controller watches **Secrets** in namespace `admin`.
* It only acts on Secrets annotated with `secret-copy/namespaces`.
* If annotation value is **empty**, the Secret is copied to **all namespaces** (except `admin`).
* Copied Secrets are labeled/annotated so the controller can track and remove stale copies.

## Important keys

* **Annotation to watch (source secret):** `secret-copy/namespaces` — comma-separated list of target namespaces; empty string → all namespaces.
* **Annotation added to copied secrets:** `secret-copy/origin: admin/<secretname>`
* **Label added to copied secrets:** `secret-copy/from=admin/<secretname>`

---

## Step-by-step flow (detailed)

1. **User action**: creates or updates a Secret in namespace `admin`.

2. **Informer / Watcher**: Controller-runtime informer receives the create/update/delete event for Secrets in `admin` (predicate filters ensure only `admin` secrets are considered).

3. **Reconciler invoked** with the Secret's namespaced name.

4. **Fetch Source Secret** from API server. If NotFound (deleted) → log and exit (no automatic deletion is performed in the current implementation).

5. **Check annotation**: Does the secret have `secret-copy/namespaces` annotation?

   * **No** → Controller ignores the secret and stops.
   * **Yes** → Continue.

6. **Parse annotation value** (CSV / comma-separated). Two cases:

   * **Empty list** (annotation exists but value is empty) → interpret as `copyToAll`.
   * **Non-empty list** → use the given target namespaces (ignore `admin` if present).

7. **If copyToAll** → list all namespaces from the API server and prepare the target set (exclude `admin`).

8. **For each target namespace** in the desired set:

   * Try to **Get** a Secret with the same name in that namespace.
   * If **NotFound** → **Create** a new Secret with the same `Type` and `Data`, and add the `secret-copy/origin` annotation and `secret-copy/from` label.
   * If **Found** → compare `Type` and `Data`:

     * If different → **Update** the existing copied Secret (also ensure annotations/labels are present).
     * If identical → do nothing.

9. **Cleanup stale copies**:

   * List Secrets across cluster that have label `secret-copy/from=admin/<sourcename>`.
   * For any such copied Secret whose namespace is **not** in the current desired set, **Delete** it.

10. **Requeue / Continue**: Reconcile returns (optionally requeue after a short interval) so periodic reconciliation keeps state consistent.

11. **Edge behaviors / notes**:

* Source secret deletion currently does **not** auto-delete copies (could be implemented).
* OwnerReferences cannot be used cross-namespace — hence the label/annotation tracking.
* RBAC must allow listing namespaces and create/update/delete secrets across the cluster.

---

## Mermaid flowchart

```
flowchart TD
  A[User: create/update Secret in admin] --> B[Informer receives event]
  B --> C[Reconciler: Get secret from admin]
  C --> D{Has annotation 'secret-copy/namespaces'?}
  D -- No --> E[Ignore secret / done]
  D -- Yes --> F[Parse annotation value]
  F --> G{Is value empty?}
  G -- Yes --> H[Set targets = all namespaces (except admin)]
  G -- No --> I[Set targets = parsed list (skip admin if present)]
  H --> J[For each target namespace]
  I --> J[For each target namespace]
  J --> K[Get secret in target ns?]
  K -- NotFound --> L[Create copied secret with label/annotation]
  K -- Found --> M{Data/Type differ?}
  M -- Yes --> N[Update copied secret (data/type/annotations/labels)]
  M -- No --> O[No action needed]
  L --> P[Continue loop]
  N --> P[Continue loop]
  O --> P[Continue loop]
  P --> Q[List all copied secrets with label secret-copy/from=admin/<name>]
  Q --> R{Copied secret's namespace in desired set?}
  R -- No --> S[Delete stale copied secret]
  R -- Yes --> T[Keep it]
  S --> U[Continue]
  T --> U[Continue]
  U --> V[Reconcile complete (optionally requeue)]
```

---

## Short legend / mapping

* **Informer** — watches resources and emits events (create/update/delete).
* **Reconciler** — core logic: ensures desired state (copies) matches declared annotation targets.
* **Desired set** — namespaces the admin secret should be copied into (computed from annotation).
* **Actual set** — namespaces where copies currently exist (found via label).
* **Sync step** — create/update in missing/out-of-date namespaces.
* **Cleanup step** — remove copies that are no longer desired.

---

## Quick checklist for each reconcile loop

* Fetch source secret
* If annotation present -> parse targets
* Create/update copies in targets
* Delete copies not in targets
* Return success (maybe requeue)


