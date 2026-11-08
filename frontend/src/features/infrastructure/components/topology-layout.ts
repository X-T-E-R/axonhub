import type { TopologyGraphResponse, TopologyNodeItem } from '../data/infrastructure';

export type TopologyLayer = 'traffic' | 'private' | 'vpn' | 'monitor';
export const layerKinds: Record<TopologyLayer, string[]> = {
  traffic: ['public_http', 'api_origin', 'private_api'],
  private: ['wireguard'],
  vpn: ['vpn', 'reality_cover'],
  monitor: ['monitor'],
};
export const isHost = (node: TopologyNodeItem) => ['server', 'node'].includes(node.kind);
const owner = (node: TopologyNodeItem) => node.nodeId || node.id.replace(/^node:/, '');

export function topologyLayout(graph: TopologyGraphResponse, layer: TopologyLayer) {
  const known = new Set(graph.nodes.map((node) => node.id));
  let connections = graph.connections.filter((edge) => layerKinds[layer].includes(edge.kind) && known.has(edge.from) && known.has(edge.to));
  if (layer === 'traffic') {
    const reached = new Set(graph.nodes.filter((node) => node.kind === 'entry' || node.kind === 'worker').map((node) => node.id));
    let changed = true;
    while (changed) {
      changed = false;
      for (const edge of connections)
        if (reached.has(edge.from) && !reached.has(edge.to)) {
          reached.add(edge.to);
          changed = true;
        }
    }
    connections = connections.filter((edge) => reached.has(edge.from) && reached.has(edge.to));
  }
  const connected = new Set(connections.flatMap((edge) => [edge.from, edge.to]));
  const nodes = graph.nodes.filter((node) => connected.has(node.id));
  const hosts = nodes.filter(isHost).sort((a, b) => owner(a).localeCompare(owner(b)));
  const positions = new Map<string, { x: number; y: number }>();
  const put = (node: TopologyNodeItem, x: number, row: number) => positions.set(node.id, { x, y: row * 108 });
  let row = 0;

  if (layer === 'traffic' || layer === 'vpn') {
    for (const host of hosts) {
      const endpoints = nodes.filter((node) => !isHost(node) && node.nodeId === owner(host));
      endpoints.forEach((node, index) => put(node, layer === 'traffic' ? 590 : node.id.startsWith('cover:') ? 700 : 0, row + index));
      put(host, layer === 'traffic' ? 920 : 350, row + (Math.max(1, endpoints.length) - 1) / 2);
      row += Math.max(1, endpoints.length);
    }
    const middle = Math.max(0, (row - 1) / 2);
    const entries = nodes.filter((node) => node.kind === 'entry');
    entries.forEach((node, index) => put(node, 0, middle + index - (entries.length - 1) / 2));
    nodes.filter((node) => node.kind === 'worker').forEach((node) => put(node, 295, middle));
  } else if (layer === 'private') {
    const targets = new Map<string, number>();
    connections.forEach((edge) => targets.set(edge.to, (targets.get(edge.to) || 0) + 1));
    const hub = [...targets].sort((a, b) => b[1] - a[1])[0]?.[0];
    const peers = nodes.filter((node) => node.id !== hub);
    peers.forEach((node, index) => put(node, 0, index));
    const hubNode = nodes.find((node) => node.id === hub);
    if (hubNode) put(hubNode, 520, Math.max(0, (peers.length - 1) / 2));
    row = peers.length;
  } else {
    hosts.forEach((node, index) => put(node, 0, index));
    nodes.filter((node) => !isHost(node)).forEach((node, index) => put(node, 520, index));
    row = hosts.length;
  }
  for (const node of nodes) if (!positions.has(node.id)) put(node, 590, row++);
  return { nodes, connections, positions, unattached: graph.nodes.filter((node) => isHost(node) && !connected.has(node.id)) };
}

// Walk only upstream: selecting one owner must never illuminate sibling origins.
export function topologyTrace(layout: ReturnType<typeof topologyLayout>, selectedId: string | null) {
  const selected = layout.nodes.find((node) => node.id === selectedId);
  const selectedOwner = selected?.nodeId;
  const ids = new Set(
    selected
      ? layout.nodes.filter((node) => node.id === selected.id || (selectedOwner && node.nodeId === selectedOwner)).map((node) => node.id)
      : []
  );
  let changed = true;
  while (changed) {
    changed = false;
    for (const edge of layout.connections)
      if (ids.has(edge.to) && !ids.has(edge.from)) {
        ids.add(edge.from);
        changed = true;
      }
  }
  return { nodes: ids, edges: new Set(layout.connections.filter((edge) => ids.has(edge.from) && ids.has(edge.to)).map((edge) => edge.id)) };
}
