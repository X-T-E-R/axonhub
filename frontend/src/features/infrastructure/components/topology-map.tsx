import { useEffect, useMemo, useState } from 'react';
import { IconCloud, IconWorld, IconServer, IconPlug, IconArrowsMaximize, IconFocus2 } from '@tabler/icons-react';
import {
  Background,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  useNodesInitialized,
  useReactFlow,
  useStore,
  type Node,
  type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Controls } from '@/components/ai-elements/controls';
import type { KomodoObservation, TopologyGraphResponse, TopologyNodeItem } from '../data/infrastructure';
import { isHost, topologyLayout, topologyTrace, type TopologyLayer } from './topology-layout';

type DiagramData = {
  item: TopologyNodeItem;
  focused: boolean;
  muted: boolean;
  detail: string;
  status: string;
  select: (id: string) => void;
};
type DiagramNode = Node<DiagramData, 'infrastructure'>;

function InfrastructureNode({ data }: NodeProps<DiagramNode>) {
  const Icon = data.item.kind === 'entry' ? IconWorld : data.item.kind === 'worker' ? IconCloud : isHost(data.item) ? IconServer : IconPlug;
  return (
    <>
      <Handle type='target' position={Position.Left} className='!border-border !bg-muted-foreground !size-1.5' />
      <button
        type='button'
        data-topology-node={data.item.id}
        onClick={() => data.select(data.item.id)}
        title={[data.item.label, data.item.address, data.status].filter(Boolean).join(' · ')}
        className={cn(
          'nodrag nopan bg-card text-card-foreground focus-visible:ring-ring w-[220px] rounded-lg border p-3 text-left shadow-sm transition-opacity outline-none focus-visible:ring-2',
          data.focused ? 'border-primary ring-primary/20 ring-2' : 'border-border',
          data.muted && 'opacity-30'
        )}
      >
        <div className='flex items-center gap-2'>
          <Icon size={18} className={cn('shrink-0', data.focused ? 'text-primary' : 'text-muted-foreground')} />
          <span className='min-w-0 truncate font-mono text-xs font-semibold'>{data.item.label}</span>
        </div>
        <div className='text-muted-foreground mt-2 truncate font-mono text-[10px]'>{data.detail}</div>
        <div className='mt-1 flex items-center justify-between gap-2 text-[10px]'>
          <span className='text-muted-foreground truncate'>{data.item.nodeId || data.item.kind}</span>
          <span className='shrink-0'>{data.status}</span>
        </div>
      </button>
      <Handle type='source' position={Position.Right} className='!border-border !bg-muted-foreground !size-1.5' />
    </>
  );
}
const nodeTypes = { infrastructure: InfrastructureNode };
const layers: TopologyLayer[] = ['traffic', 'private', 'vpn', 'monitor'];

function FitCanvasSize() {
  const width = useStore((state) => state.width);
  const height = useStore((state) => state.height);
  const initialized = useNodesInitialized();
  const { fitView } = useReactFlow();
  useEffect(() => {
    if (initialized && width && height) void fitView({ padding: 0.12, maxZoom: 1, duration: 0 });
  }, [initialized, width, height, fitView]);
  return null;
}

export function TopologyMap({
  graph,
  komodo,
  onSelectNode,
}: {
  graph?: TopologyGraphResponse;
  komodo: Record<string, KomodoObservation>;
  onSelectNode: (id: string) => void;
}) {
  const { t } = useTranslation();
  const [layer, setLayer] = useState<TopologyLayer>('traffic');
  const [focused, setFocused] = useState<string | null>(null);
  const [edgeId, setEdgeId] = useState<string | null>(null);
  const [expanded, setExpanded] = useState(false);
  useEffect(() => {
    setFocused(null);
    setEdgeId(null);
  }, [graph?.source, graph?.version, graph?.revision, layer]);
  const layout = useMemo(
    () => topologyLayout(graph ?? { nodes: [], connections: [], source: '', version: '', revision: 0 }, layer),
    [graph, layer]
  );
  const trace = useMemo(() => topologyTrace(layout, focused), [layout, focused]);
  const selectedEdge = layout.connections.find((edge) => edge.id === edgeId);
  const selectedItem = layout.nodes.find((node) => node.id === focused);
  const select = (id: string) => {
    setFocused(id);
    setEdgeId(null);
    const item = layout.nodes.find((node) => node.id === id);
    if (item?.nodeId) onSelectNode(item.nodeId);
  };
  const nodes: DiagramNode[] = layout.nodes.map((item) => {
    const observation = item.nodeId ? komodo[item.nodeId] : null;
    const status =
      isHost(item) && observation?.state
        ? t(
            `infrastructure.status.${observation.state === 'Ok' ? 'ok' : observation.state === 'NotOk' ? 'notOk' : observation.state === 'Disabled' ? 'disabled' : 'unregistered'}`
          )
        : t(
            `infrastructure.graph.status.${item.status === 'active' || item.status === 'published' ? 'active' : item.status === 'verified' ? 'verified' : 'configured'}`
          );
    return {
      id: item.id,
      type: 'infrastructure',
      position: layout.positions.get(item.id)!,
      data: {
        item,
        select,
        detail: isHost(item)
          ? observation?.publicIp || item.address || ''
          : item.address || (item.kind === 'worker' ? graph?.version?.slice(0, 8) : '') || '',
        status,
        focused: focused === item.id || Boolean(selectedEdge && [selectedEdge.from, selectedEdge.to].includes(item.id)),
        muted: Boolean((focused && !trace.nodes.has(item.id)) || (selectedEdge && ![selectedEdge.from, selectedEdge.to].includes(item.id))),
      },
    };
  });
  const edges = layout.connections.map((item) => {
    const highlighted = trace.edges.has(item.id) || edgeId === item.id;
    const color = highlighted ? 'var(--primary)' : 'var(--muted-foreground)';
    return {
      id: item.id,
      source: item.from,
      target: item.to,
      type: 'bezier',
      label:
        item.kind === 'public_http'
          ? 'HTTPS / WSS'
          : item.kind === 'private_api'
            ? t('infrastructure.graph.privateLane')
            : layer === 'traffic'
              ? 'API'
              : item.kind === 'wireguard'
                ? 'WireGuard'
                : undefined,
      markerEnd: { type: MarkerType.ArrowClosed, color, width: 16, height: 16 },
      style: {
        stroke: color,
        strokeWidth: highlighted ? 2.5 : 1.4,
        strokeDasharray: ['proposed', 'configured'].includes(item.status) ? '5 4' : undefined,
        opacity: (focused && !highlighted) || (selectedEdge && edgeId !== item.id) ? 0.16 : 0.85,
      },
      labelStyle: { fill: color, fontSize: 10 },
      labelBgStyle: { fill: 'var(--background)', fillOpacity: 0.95 },
    };
  });
  const canvas = (large = false) => (
    <div
      className={cn('bg-muted/10 relative overflow-hidden rounded-lg border', large ? 'min-h-0 flex-1' : 'h-[460px] sm:h-[550px]')}
      data-testid={large ? 'topology-map-expanded' : 'topology-map'}
    >
      {nodes.length ? (
        <ReactFlow<DiagramNode>
          key={`${layer}-${large}`}
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          fitView
          fitViewOptions={{ padding: 0.12, maxZoom: 1 }}
          minZoom={0.25}
          maxZoom={1.8}
          nodesDraggable={false}
          nodesConnectable={false}
          nodesFocusable={false}
          edgesFocusable
          deleteKeyCode={null}
          panOnDrag
          zoomOnDoubleClick={false}
          proOptions={{ hideAttribution: true }}
          onPaneClick={() => {
            setFocused(null);
            setEdgeId(null);
          }}
          onEdgeClick={(_, edge) => {
            setEdgeId(edge.id);
            setFocused(null);
          }}
          aria-label={t('infrastructure.graph.label')}
        >
          <FitCanvasSize />
          <Background bgColor='var(--background)' color='var(--border)' gap={22} size={1} />
          <Controls
            className='[&_button]:!text-foreground [&_svg]:fill-current'
            showInteractive={false}
            position='bottom-left'
            aria-label={t('infrastructure.graph.zoom')}
          />
        </ReactFlow>
      ) : (
        <div className='text-muted-foreground flex h-full items-center justify-center p-6 text-sm'>{t('infrastructure.graph.empty')}</div>
      )}
    </div>
  );
  const toolbar = (
    <div className='flex flex-wrap items-center justify-between gap-2'>
      <div className='bg-muted flex max-w-full flex-wrap gap-1 rounded-lg p-1' aria-label={t('infrastructure.graph.layers')}>
        {layers.map((value) => (
          <Button
            key={value}
            size='sm'
            variant={layer === value ? 'secondary' : 'ghost'}
            className='h-7 px-2.5 text-xs'
            aria-pressed={layer === value}
            onClick={() => setLayer(value)}
          >
            {t(`infrastructure.graph.layer.${value}`)}
          </Button>
        ))}
      </div>
      <div className='flex items-center gap-1'>
        {focused || edgeId ? (
          <Button
            size='sm'
            variant='ghost'
            className='h-7 text-xs'
            onClick={() => {
              setFocused(null);
              setEdgeId(null);
            }}
          >
            <IconFocus2 size={14} />
            {t('infrastructure.graph.clear')}
          </Button>
        ) : null}
        {!expanded ? (
          <Button size='sm' variant='outline' className='h-7 text-xs' onClick={() => setExpanded(true)}>
            <IconArrowsMaximize size={14} />
            {t('infrastructure.graph.expand')}
          </Button>
        ) : null}
      </div>
    </div>
  );
  const detail = (
    <div className='text-muted-foreground min-h-8 text-xs leading-relaxed' aria-live='polite'>
      {selectedEdge ? (
        <>
          <span className='text-foreground font-medium'>{selectedEdge.label || selectedEdge.kind}</span> ·{' '}
          {selectedEdge.evidence || t('infrastructure.graph.configOnly')}
          {selectedEdge.observedAt ? ` · ${new Date(selectedEdge.observedAt).toLocaleString()}` : ''}
        </>
      ) : selectedItem ? (
        <>
          <span className='text-foreground font-mono font-medium'>{selectedItem.label}</span> ·{' '}
          {selectedItem.address || selectedItem.nodeId} · {t('infrastructure.graph.pathHighlight')}
        </>
      ) : (
        t('infrastructure.graph.help')
      )}
    </div>
  );
  return (
    <div className='space-y-3'>
      {toolbar}
      {canvas()}
      {detail}
      <div className='text-muted-foreground flex flex-wrap items-center gap-x-4 gap-y-2 text-[11px]'>
        <span className='flex items-center gap-1.5'>
          <i className='border-muted-foreground w-5 border-t' />
          {t('infrastructure.graph.bound')}
        </span>
        <span className='flex items-center gap-1.5'>
          <i className='border-muted-foreground w-5 border-t border-dashed' />
          {t('infrastructure.graph.configOnly')}
        </span>
        <span className='font-mono'>
          {layout.nodes.length} {t('infrastructure.graph.nodes')} · {layout.connections.length} {t('infrastructure.graph.links')}
        </span>
      </div>
      {layout.unattached.length ? (
        <details className='border-t pt-2 text-xs'>
          <summary className='text-muted-foreground cursor-pointer'>
            {t('infrastructure.graph.unattached', { count: layout.unattached.length })}
          </summary>
          <div className='mt-2 flex flex-wrap gap-2'>
            {layout.unattached.map((node) => (
              <Button
                key={node.id}
                variant='outline'
                size='sm'
                className='h-7 font-mono text-[11px]'
                onClick={() => node.nodeId && onSelectNode(node.nodeId)}
              >
                {node.label}
              </Button>
            ))}
          </div>
        </details>
      ) : null}
      <Dialog open={expanded} onOpenChange={setExpanded}>
        <DialogContent className='flex h-[92dvh] max-w-[96vw] flex-col gap-3 p-4 sm:max-w-[96vw]'>
          <DialogHeader>
            <DialogTitle>{t('infrastructure.tabs.topology')}</DialogTitle>
            <DialogDescription>{t('infrastructure.graph.help')}</DialogDescription>
          </DialogHeader>
          {toolbar}
          {canvas(true)}
          {detail}
        </DialogContent>
      </Dialog>
    </div>
  );
}
