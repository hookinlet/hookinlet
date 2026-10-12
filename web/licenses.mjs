// Writes THIRD_PARTY_LICENSES for the console: the license text of every npm package that
// is in the production build (recorded by the build itself, in vite.config.ts). The
// release's own file, scripts/third-party-licenses.sh, appends this one. Needs `npm ci`
// and `npm run build` first. Development tools that do not ship are not listed.
import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

// OFL-1.1 is the licence of the fonts, which ship as files, not as code.
const ALLOWED = new Set(['MIT', 'ISC', 'BSD-2-Clause', 'BSD-3-Clause', 'Apache-2.0', 'OFL-1.1'])
// The build lists the packages whose code is in the bundle; the direct dependencies are
// added to it, because a package that only re-exports (vue) leaves no module behind.
const direct = Object.keys(JSON.parse(readFileSync('package.json', 'utf8')).dependencies ?? {})
const bundled = [...new Set([...JSON.parse(readFileSync('node_modules/.cache/bundled-packages.json', 'utf8')), ...direct])].sort()
if (bundled.length === 0) {
  console.error('licenses: the build recorded no packages; run npm run build first')
  process.exit(1)
}

let out = ''
for (const name of bundled) {
  const dir = join('node_modules', name)
  const pkg = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'))
  const kind = typeof pkg.license === 'string' ? pkg.license : (pkg.license?.type ?? '')
  if (!ALLOWED.has(kind)) {
    console.error(`licenses: ${name} is under "${kind}", which is not on the allowed list (${[...ALLOWED].join(', ')})`)
    process.exit(1)
  }
  const files = readdirSync(dir).filter((f) => /^(LICEN[CS]E|NOTICE|COPYING)/i.test(f)).sort()
  if (files.length === 0) {
    console.error(`licenses: no license file in ${dir}`)
    process.exit(1)
  }
  out += `\n${'='.repeat(80)}\n${name}@${pkg.version} (npm)\nLicense: ${kind}\n${'='.repeat(80)}\n`
  for (const f of files) {
    if (existsSync(join(dir, f))) out += `\n--- ${f} ---\n${readFileSync(join(dir, f), 'utf8')}\n`
  }
}
writeFileSync('THIRD_PARTY_LICENSES', out.replace(/^\n/, ''))
console.log(`wrote THIRD_PARTY_LICENSES (${bundled.length} packages)`)
