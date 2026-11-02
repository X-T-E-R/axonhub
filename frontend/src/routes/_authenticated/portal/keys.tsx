import { createFileRoute } from '@tanstack/react-router';
import PortalKeys from '@/features/portal/keys';

export const Route = createFileRoute('/_authenticated/portal/keys')({ component: PortalKeys });
