import { flexRender, type Table as TableInstance, type RowData } from '@tanstack/react-table';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { TableSkeleton } from '@/components/ui/table-skeleton';

declare module '@tanstack/react-table' {
  interface ColumnMeta<TData extends RowData, TValue> {
    className: string;
  }
}

export function DataTableFrame<T>({
  table,
  loading = false,
  pageSize = 10,
  className,
}: {
  table: TableInstance<T>;
  loading?: boolean;
  pageSize?: number;
  className?: string;
}) {
  const { t } = useTranslation();
  const columns = table.getVisibleLeafColumns().length;
  return (
    <div
      className={cn('shadow-soft relative mt-4 min-h-0 flex-1 overflow-auto rounded-2xl border border-[var(--table-border)]', className)}
    >
      <Table className='border-separate border-spacing-0 rounded-2xl bg-[var(--table-background)]'>
        <TableHeader className='sticky top-0 z-20 bg-[var(--table-header)] shadow-sm'>
          {table.getHeaderGroups().map((headerGroup) => (
            <TableRow key={headerGroup.id} className='group/row border-0'>
              {headerGroup.headers.map((header) => (
                <TableHead
                  key={header.id}
                  colSpan={header.colSpan}
                  className={cn(
                    header.column.columnDef.meta?.className,
                    'text-muted-foreground border-0 text-xs font-semibold tracking-wider uppercase'
                  )}
                >
                  {header.isPlaceholder ? null : flexRender(header.column.columnDef.header, header.getContext())}
                </TableHead>
              ))}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody className='space-y-1 !bg-[var(--table-background)] p-2'>
          {loading ? (
            <TableSkeleton rows={pageSize} columns={columns} />
          ) : table.getRowModel().rows.length ? (
            table.getRowModel().rows.map((row) => (
              <TableRow
                key={row.id}
                data-state={row.getIsSelected() && 'selected'}
                className='group/row table-row-hover rounded-xl border-0 !bg-[var(--table-background)] transition-colors duration-200'
              >
                {row.getVisibleCells().map((cell) => (
                  <TableCell key={cell.id} className={cn(cell.column.columnDef.meta?.className, 'border-0 bg-inherit px-4 py-3')}>
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </TableCell>
                ))}
              </TableRow>
            ))
          ) : (
            <TableRow className='!bg-[var(--table-background)]'>
              <TableCell colSpan={columns} className='h-24 text-center'>
                {t('common.noData')}
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </div>
  );
}
