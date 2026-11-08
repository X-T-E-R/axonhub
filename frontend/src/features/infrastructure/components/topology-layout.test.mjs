import assert from 'node:assert/strict';
import test from 'node:test';
import { topologyLayout, topologyTrace } from './topology-layout.ts';

const graph = {
  source: 'production',
  version: 'accepted-worker',
  revision: null,
  nodes: [
    { id: 'entry:public', kind: 'entry', label: 'Public HTTPS' },
    { id: 'worker', kind: 'worker', label: 'Worker' },
    ...['a', 'b', 'unattached'].map((id) => ({ id: 'node:' + id, kind: 'server', nodeId: id, label: id })),
    ...['a', 'b'].map((id) => ({ id: 'api:' + id, kind: 'endpoint', nodeId: id, label: 'API' })),
    { id: 'private:a', kind: 'endpoint', nodeId: 'a', label: 'Private Messages' },
    { id: 'monitor:a', kind: 'endpoint', nodeId: 'a', label: 'Komodo Core' },
  ],
  connections: [
    { id: 'ingress', from: 'entry:public', to: 'worker', kind: 'public_http', status: 'active' },
    ...['a', 'b'].flatMap((id) => [
      { id: 'dispatch:' + id, from: 'worker', to: 'api:' + id, kind: 'api_origin', status: 'active' },
      { id: 'local:' + id, from: 'api:' + id, to: 'node:' + id, kind: 'api_origin', status: 'configured' },
    ]),
    { id: 'private', from: 'worker', to: 'private:a', kind: 'private_api', status: 'configured' },
    { id: 'private-host', from: 'private:a', to: 'node:a', kind: 'private_api', status: 'configured' },
    { id: 'monitor', from: 'node:a', to: 'monitor:a', kind: 'monitor', status: 'configured' },
    { id: 'wg', from: 'node:b', to: 'node:a', kind: 'wireguard', status: 'verified' },
    { id: 'dangling', from: 'worker', to: 'not-recorded', kind: 'api_origin', status: 'configured' },
  ],
};

test('traffic layer retains exact recorded paths, private owner and separate unattached hosts without fabricating links', () => {
  const before = structuredClone(graph);
  const view = topologyLayout(graph, 'traffic');
  assert.equal(view.connections.length, 7);
  assert.deepEqual(
    view.unattached.map((node) => node.id),
    ['node:unattached']
  );
  assert(!view.nodes.some((node) => node.id === 'monitor:a'));
  assert(view.connections.some((edge) => edge.id === 'private-host' && edge.to === 'node:a'));
  assert.equal(view.positions.size, view.nodes.length);
  assert(view.nodes.every((node) => Object.values(view.positions.get(node.id)).every(Number.isFinite)));
  assert(view.positions.get('worker').x < view.positions.get('api:a').x);
  assert(view.positions.get('api:a').x < view.positions.get('node:a').x);
  assert.deepEqual(graph, before);
});

test('owner selection traces upstream to ingress without highlighting sibling destinations', () => {
  const view = topologyLayout(graph, 'traffic');
  const trace = topologyTrace(view, 'node:a');
  assert.deepEqual([...trace.edges].sort(), ['dispatch:a', 'ingress', 'local:a', 'private', 'private-host'].sort());
  assert(!trace.nodes.has('api:b'));
  assert(!trace.nodes.has('node:b'));
  assert.equal(topologyTrace(view, null).edges.size, 0);
});

test('private and monitoring layers use their own actual connections and tolerate empty data', () => {
  const network = topologyLayout(graph, 'private');
  assert.deepEqual(
    network.connections.map((edge) => edge.id),
    ['wg']
  );
  assert(network.positions.get('node:a').x > network.positions.get('node:b').x);
  assert.deepEqual(
    topologyLayout(graph, 'monitor').connections.map((edge) => edge.id),
    ['monitor']
  );
  assert.equal(topologyLayout(graph, 'vpn').nodes.length, 0);
  assert.equal(topologyLayout({ ...graph, nodes: [], connections: [] }, 'traffic').nodes.length, 0);
});

test('a configured local API-to-host link alone does not imply attachment to the public Worker', () => {
  const value = structuredClone(graph);
  value.nodes.push({ id: 'api:unattached', kind: 'endpoint', nodeId: 'unattached', label: 'Prepared API' });
  value.connections.push({ id: 'prepared-only', from: 'api:unattached', to: 'node:unattached', kind: 'api_origin', status: 'configured' });
  const view = topologyLayout(value, 'traffic');
  assert(!view.connections.some((edge) => edge.id === 'prepared-only'));
  assert(!view.nodes.some((node) => node.id === 'node:unattached'));
  assert(view.unattached.some((node) => node.id === 'node:unattached'));
});
