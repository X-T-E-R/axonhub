import { type ColumnDef, getCoreRowModel, useReactTable } from '@tanstack/react-table';
import { DataTableFrame } from '@/components/data-table-frame';

export function PortalTable<T>({ data, columns }: { data: T[]; columns: ColumnDef<T>[] }) {
  const table = useReactTable({ data, columns, getCoreRowModel: getCoreRowModel() });
  return <DataTableFrame table={table} className='mt-0 max-w-full' />;
}
