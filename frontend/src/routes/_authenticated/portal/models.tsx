import { createFileRoute } from '@tanstack/react-router';
import PortalModels from '@/features/portal/models';

export const Route = createFileRoute('/_authenticated/portal/models')({ component: PortalModels });
