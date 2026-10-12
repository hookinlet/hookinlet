// Writes THIRD_PARTY_LICENSES for the console: the license text of every package its
// production build can contain (the Vue runtime and router and what they depend on).
// scripts/third-party-licenses.sh appends this file to the one the release carries.
// Needs `npm ci` first. Development tools do not ship, so they are not listed.
import { execFileSync } from 'node:child_process'
import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const ALLOWED = new Set(['MIT', 'ISC', 'BSD-2-Clause', 'BSD-3-Clause', 'Apache-2.0'])

const tree = JSON.parse(execFileSync('npm', ['ls', '--omit=dev', '--all', '--json'], { encoding: 'utf8', maxBuffer: 1 << 26 }))
const dev = new Set(Object.keys(JSON.parse(readFileSync('package.json', 'utf8')).devDependencies ?? {}))
const names = new Set()
const walk = (deps) => {
  for (const [name, d] of Object.entries(deps ?? {})) {
    if (!dev.has(name)) names.add(name) // a tool that is also an optional peer of vue is not shipped
    walk(d.dependencies)
  }
}
walk(tree.dependencies)

let out = ''
for (const name of [...names].sort()) {
  const dir = join('node_modules', name)
  if (!existsSync(join(dir, 'package.json'))) continue // an optional peer that is not installed
  const pkg = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'))
  const kind = typeof pkg.license === 'string' ? pkg.license : (pkg.license?.type ?? '')
  if (!ALLOWED.has(kind)) {
    console.error(`licenses: ${name} is under "${kind}", which is not on the allowed list (${[...ALLOWED].join(', ')})`)
    process.exit(1)
  }
  const usable = readdirSync(dir).filter((f) => /^(LICEN[CS]E|NOTICE|COPYING)/i.test(f)).sort()
  if (usable.length === 0) {
    console.error(`licenses: no license file in ${dir}`)
    process.exit(1)
  }
  out += `\n${'='.repeat(80)}\n${name}@${pkg.version} (npm)\nLicense: ${kind}\n${'='.repeat(80)}\n`
  for (const f of usable) {
    if (!existsSync(join(dir, f))) continue
    out += `\n--- ${f} ---\n${readFileSync(join(dir, f), 'utf8')}\n`
  }
}
writeFileSync('THIRD_PARTY_LICENSES', out.replace(/^\n/, ''))
console.log(`wrote THIRD_PARTY_LICENSES (${names.size} packages)`)
