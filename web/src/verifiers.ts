// The ways an inbox can check a delivery, as the new-inbox and settings forms offer them. The
// server is the authority on what is valid (it checks the configuration when it is saved);
// this only shapes the form and builds the configuration object.

export interface Field {
  key: string
  label: string
  help: string
  kind: 'text' | 'number' | 'select'
  options?: string[]
  required?: boolean
  placeholder?: string
}

export interface Preset {
  id: string
  label: string
  summary: string
  secret: false | { label: string; placeholder: string }
  fields: Field[]
}

const tolerance: Field = { key: 'tolerance_seconds', label: 'Timestamp tolerance (seconds)', help: 'How far a signed timestamp may be from this server’s clock. Default 300.', kind: 'number', placeholder: '300' }

export const PRESETS: Preset[] = [
  { id: 'stripe', label: 'Stripe', summary: 'Stripe-Signature, with a timestamp', secret: { label: 'Signing secret', placeholder: 'whsec_…' }, fields: [tolerance] },
  { id: 'github', label: 'GitHub', summary: 'X-Hub-Signature-256', secret: { label: 'Webhook secret', placeholder: 'the secret set on the webhook' }, fields: [] },
  { id: 'slack', label: 'Slack', summary: 'X-Slack-Signature; answers the URL handshake', secret: { label: 'Signing secret', placeholder: 'from the app’s Basic Information page' }, fields: [tolerance] },
  { id: 'standardwebhooks', label: 'Standard Webhooks', summary: 'Svix and compatible senders', secret: { label: 'Signing secret', placeholder: 'whsec_…' }, fields: [tolerance] },
  {
    id: 'hmac',
    label: 'HMAC',
    summary: 'Any sender that signs the body with a shared secret',
    secret: { label: 'Shared secret', placeholder: 'the secret both sides know' },
    fields: [
      { key: 'header', label: 'Signature header', help: 'The header that carries the signature.', kind: 'text', required: true, placeholder: 'X-Signature' },
      { key: 'algorithm', label: 'Algorithm', help: 'Default sha256.', kind: 'select', options: ['', 'sha1', 'sha256', 'sha384', 'sha512'] },
      { key: 'encoding', label: 'Encoding', help: 'Default hex.', kind: 'select', options: ['', 'hex', 'base64', 'base64url'] },
      { key: 'prefix', label: 'Prefix', help: 'Literal text before the signature, such as sha256=.', kind: 'text', placeholder: 'sha256=' },
      { key: 'timestamp_header', label: 'Timestamp header', help: 'A header with a Unix timestamp. Turns on the tolerance check.', kind: 'text', placeholder: 'X-Timestamp' },
      { key: 'delivery_id_header', label: 'Delivery id header', help: 'Retries with the same value are stored once. It is not authenticated.', kind: 'text', placeholder: 'X-Delivery-Id' },
      tolerance,
    ],
  },
  {
    id: 'token',
    label: 'Shared token',
    summary: 'A header that must equal a secret. Weaker than a signature.',
    secret: { label: 'Token', placeholder: 'the value the sender puts in the header' },
    fields: [{ key: 'header', label: 'Header', help: 'Default X-Inbox-Token.', kind: 'text', placeholder: 'X-Inbox-Token' }],
  },
  { id: 'none', label: 'None', summary: 'Accepts anything and stores it as not verified', secret: false, fields: [] },
]

export function presetOf(id: string): Preset | undefined {
  return PRESETS.find((p) => p.id === id)
}

// buildConfig turns the form's values into the verifier_config object: only fields that were
// filled in are sent, numbers as numbers.
export function buildConfig(preset: Preset, values: Record<string, string>): Record<string, string | number> {
  const out: Record<string, string | number> = {}
  for (const f of preset.fields) {
    const raw = (values[f.key] ?? '').trim()
    if (raw === '') continue
    out[f.key] = f.kind === 'number' ? Number(raw) : raw
  }
  return out
}

// missing names the required fields that were left empty.
export function missing(preset: Preset, values: Record<string, string>): string[] {
  return preset.fields.filter((f) => f.required && (values[f.key] ?? '').trim() === '').map((f) => f.label)
}

// valuesOf is the reverse of buildConfig, for editing an inbox that already has a configuration.
export function valuesOf(config: Record<string, unknown> | undefined): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(config ?? {})) out[k] = String(v)
  return out
}
