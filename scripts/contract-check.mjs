import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const spec = JSON.parse(readFileSync(path.join(root, 'contracts/openapi.json'), 'utf8'));
assert.equal(spec.openapi, '3.0.3');
const methods = new Set(['get', 'put', 'post', 'delete', 'patch', 'head', 'options']);
const operationIds = new Set();
let operations = 0;
function walk(value) {
  if (!value || typeof value !== 'object') return;
  if (value.$ref) {
    assert.ok(value.$ref.startsWith('#/'), '契约不允许远端引用');
    const target = value.$ref.slice(2).split('/').reduce((parent, key) => parent?.[key.replace(/~1/g, '/').replace(/~0/g, '~')], spec);
    assert.ok(target, `引用不存在：${value.$ref}`);
  }
  if (value.type === 'object' && value.properties) {
    assert.equal(value.additionalProperties, false);
    for (const name of value.required ?? []) assert.ok(name in value.properties, `必填字段不存在：${name}`);
  }
  for (const child of Object.values(value)) walk(child);
}
walk(spec);
for (const [url, item] of Object.entries(spec.paths)) {
  assert.ok(url.startsWith('/api/v1/'), `管理路径无版本：${url}`);
  for (const [method, operation] of Object.entries(item)) {
    if (!methods.has(method)) continue;
    operations++;
    assert.ok(operation.operationId && !operationIds.has(operation.operationId), `operationId 缺失或重复：${url}`);
    operationIds.add(operation.operationId);
    assert.ok(operation.responses['200'], `${method} ${url} 缺少成功响应`);
    assert.ok(operation.responses['401'], `${method} ${url} 缺少认证失效响应`);
    for (const [status, response] of Object.entries(operation.responses)) {
      if (Number(status) >= 400) assert.equal(response.content?.['application/json']?.schema?.$ref, '#/components/schemas/ErrorResponse');
    }
    if (method === 'get') {
      const schema = operation.responses['200'].content['application/json'].schema;
      const serialized = JSON.stringify(schema.$ref ? spec.components.schemas[schema.$ref.split('/').at(-1)] : schema);
      assert.ok(!serialized.includes('"key"'), `${url} 读取响应包含密钥字段`);
    }
  }
}
assert.equal(spec.components.schemas.CredentialInput.properties.key.writeOnly, true);
execFileSync(process.execPath, [path.join(root, 'scripts/generate-contract.mjs'), '--check'], { cwd: root, stdio: 'inherit' });
console.log(`OpenAPI 结构、本地引用和 ${operations} 个操作的生成一致性通过；实际响应另由 Go 契约测试验证。`);
