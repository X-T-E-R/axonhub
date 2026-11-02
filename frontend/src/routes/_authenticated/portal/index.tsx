import { createFileRoute } from '@tanstack/react-router';
import PortalOverview from '@/features/portal/overview';

export const Route = createFileRoute('/_authenticated/portal/')({ component: PortalOverview });
