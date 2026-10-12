<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ApiError, backup, runRetention } from '../api/client'
import { ago, bytes, compact } from '../format'
import { guard } from '../session'
import { status } from '../store'

const s = computed(() => status.data.value)
const BIG = 500_000_000 // past this, a copy through the browser is the wrong tool

const retentionBusy = ref(false)
const retentionMsg = ref('')
const retentionErr = ref('')
async function retain() {
  retentionBusy.value = true
  retentionMsg.value = ''
  retentionErr.value = ''
  try {
    const r = await guard(runRetention)
    retentionMsg.value = r.deleted === 0 ? 'Done. Nothing was due to be deleted.' : `Done. Deleted ${compact(r.deleted)} message${r.deleted === 1 ? '' : 's'}.`
    void status.refresh()
  } catch (e) {
    retentionErr.value = e instanceof ApiError && e.status === 409 ? 'A retention run is already in progress.' : e instanceof ApiError ? e.message : 'Could not run retention.'
  } finally {
    retentionBusy.value = false
  }
}

const backupBusy = ref(false)
const backupErr = ref('')
const backupMsg = ref('')
async function download() {
  backupBusy.value = true
  backupErr.value = ''
  backupMsg.value = ''
  try {
    const blob = await guard(backup)
    const stamp = new Date().toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15)
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `hookinlet-backup-${stamp}.db`
    document.body.appendChild(a)
    a.click()
    a.remove()
    window.setTimeout(() => URL.revokeObjectURL(url), 10_000)
    backupMsg.value = `Downloaded ${bytes(blob.size)}.`
  } catch (e) {
    backupErr.value = e instanceof ApiError && e.status === 409 ? 'A backup is already running.' : e instanceof ApiError && e.status === 501 ? 'This server’s store cannot make backups.' : e instanceof ApiError ? e.message : 'Could not take the backup.'
  } finally {
    backupBusy.value = false
  }
}

onMounted(() => void status.refresh())
</script>

<template>
  <h1>Maintenance</h1>

  <section class="card sect" aria-labelledby="m-status">
    <h2 id="m-status">Server</h2>
    <dl v-if="s" class="kv">
      <dt>Version</dt><dd class="mono">{{ s.version }}</dd>
      <dt>Database schema</dt><dd class="mono">{{ s.schema_version }}</dd>
      <dt>Database size</dt><dd>{{ bytes(s.database_bytes) }}</dd>
      <dt>Free on this volume</dt><dd>{{ s.free_bytes === undefined ? 'unknown' : bytes(s.free_bytes) }}</dd>
      <dt>Retention schedule</dt><dd>{{ s.retention_interval_seconds > 0 ? `every ${s.retention_interval_seconds >= 3600 ? `${Math.round(s.retention_interval_seconds / 3600)} h` : `${Math.round(s.retention_interval_seconds / 60)} min`}` : 'off' }}</dd>
      <dt>Last retention run</dt><dd>{{ s.last_retention_at ? `${ago(s.last_retention_at)}, deleted ${compact(s.last_retention_deleted ?? 0)}` : 'none since this server started' }}</dd>
    </dl>
    <div v-else class="skeleton line"></div>
  </section>

  <section class="card sect" aria-labelledby="m-ret">
    <h2 id="m-ret">Retention</h2>
    <p class="muted">Deletes the messages each inbox no longer keeps, now, instead of waiting for the schedule.</p>
    <div class="row">
      <button type="button" class="btn primary" :disabled="retentionBusy" @click="retain">{{ retentionBusy ? 'Running…' : 'Run retention now' }}</button>
      <span v-if="retentionMsg" class="ok" role="status">{{ retentionMsg }}</span>
      <span v-if="retentionErr" class="err" role="alert">{{ retentionErr }}</span>
    </div>
  </section>

  <section class="card sect" aria-labelledby="m-bk">
    <h2 id="m-bk">Backup</h2>
    <p class="muted">A consistent copy of the whole database: every inbox, message, sealed secret and token hash. It is as sensitive as the database. Secrets in it open only with this server’s master key, so keep the key with the backup, but apart from it. After restoring from it, run <code>hookinlet renew-epochs</code> with the server stopped.</p>
    <p v-if="s && s.database_bytes > BIG" class="warn">The database is {{ bytes(s.database_bytes) }}. Run <code>hookinlet backup --out FILE</code> on the server instead of copying it through the browser.</p>
    <div class="row">
      <button type="button" class="btn primary" :disabled="backupBusy || (s !== null && s.database_bytes > BIG)" @click="download">{{ backupBusy ? 'Preparing…' : 'Download a backup' }}</button>
      <span v-if="backupMsg" class="ok" role="status">{{ backupMsg }}</span>
      <span v-if="backupErr" class="err" role="alert">{{ backupErr }}</span>
    </div>
  </section>
</template>

<style scoped>
h1 { font-size: 22px; margin-bottom: 18px; }
.sect { padding: 20px; display: grid; gap: 14px; max-width: 680px; margin-bottom: 16px; }
.sect h2 { font-size: 16px; }
.kv { display: grid; grid-template-columns: 170px minmax(0, 1fr); gap: 8px 14px; margin: 0; font-size: 13.5px; }
.kv dt { color: var(--mute); }
.kv dd { margin: 0; }
.row { display: flex; gap: 14px; align-items: center; flex-wrap: wrap; }
.ok { color: var(--ok); font-size: 13px; }
.warn { color: var(--warn); font-size: 13px; }
.line { height: 8rem; }
</style>
