const task = require('azure-pipelines-task-lib/task');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const child = require('child_process');

(async () => {
  try {
    if (process.platform !== 'linux' || !['x64', 'arm64'].includes(process.arch)) throw new Error('unsupported Azure agent platform');
    const version = task.getInput('binaryVersion', true);
    const expected = task.getInput('binarySha256', true);
    if (!/^v\d+\.\d+\.\d+$/.test(version) || !/^[0-9a-f]{64}$/.test(expected)) throw new Error('invalid immutable binary identity');
    const arch = process.arch === 'x64' ? 'amd64' : 'arm64';
    const name = `autosql-${version}-linux-${arch}`;
    const response = await fetch(`https://github.com/stigenai/autosql/releases/download/${version}/${name}.tar.gz`);
    if (!response.ok) throw new Error('released AutoSQL archive is unavailable');
    const archive = Buffer.from(await response.arrayBuffer());
    if (crypto.createHash('sha256').update(archive).digest('hex') !== expected) throw new Error('released AutoSQL archive checksum mismatch');
    const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'autosql-'));
    const archivePath = path.join(directory, `${name}.tar.gz`);
    fs.writeFileSync(archivePath, archive, {mode: 0o600});
    child.execFileSync('tar', ['-xzf', archivePath, '-C', directory], {stdio: 'ignore'});
    const binary = path.join(directory, name);
    fs.chmodSync(binary, 0o700);
    const result = child.spawnSync(binary, ['integration', task.getInput('mode', true), '--contract', task.getPathInput('contract', true, true), '--contract-digest', task.getInput('contractDigest', true), '--json'], {stdio: 'inherit', env: process.env});
    if (result.status !== 0) throw new Error('AutoSQL refused the operation');
    task.setResult(task.TaskResult.Succeeded, 'AutoSQL completed');
  } catch (_) {
    task.setResult(task.TaskResult.Failed, 'AutoSQL task failed');
  }
})();
