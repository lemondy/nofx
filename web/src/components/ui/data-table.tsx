import * as React from 'react'
import { cn } from '../../lib/cn'
import { EmptyState } from './empty-state'
import { Skeleton } from './skeleton'

export interface DataTableColumn<T> {
  key: string
  header: React.ReactNode
  /** 单元格渲染; 缺省时取 row[key] */
  render?: (row: T, index: number) => React.ReactNode
  /** 数字列: 右对齐 + .num 等宽数字 */
  numeric?: boolean
  align?: 'left' | 'right' | 'center'
  width?: number | string
  className?: string
  headerClassName?: string
}

export interface DataTableProps<T> {
  columns: DataTableColumn<T>[]
  data: T[]
  rowKey: (row: T, index: number) => string | number
  loading?: boolean
  /** 加载态骨架行数 */
  skeletonRows?: number
  /** 空态内容, 缺省为 EmptyState */
  empty?: React.ReactNode
  emptyText?: React.ReactNode
  onRowClick?: (row: T, index: number) => void
  /** 表头吸顶(需要 maxHeight 才有滚动容器) */
  stickyHeader?: boolean
  maxHeight?: number | string
  className?: string
}

export function DataTable<T>({
  columns,
  data,
  rowKey,
  loading = false,
  skeletonRows = 5,
  empty,
  emptyText,
  onRowClick,
  stickyHeader = true,
  maxHeight,
  className,
}: DataTableProps<T>) {
  const alignOf = (c: DataTableColumn<T>) =>
    c.align ?? (c.numeric ? 'right' : 'left')
  const alignClass = (a: 'left' | 'right' | 'center') =>
    a === 'right' ? 'text-right' : a === 'center' ? 'text-center' : 'text-left'

  const showEmpty = !loading && data.length === 0

  return (
    <div
      className={cn('overflow-auto', className)}
      style={maxHeight != null ? { maxHeight } : undefined}
    >
      <table className="w-full border-collapse text-[13px]">
        <thead>
          <tr className="border-b border-line">
            {columns.map((c) => (
              <th
                key={c.key}
                style={c.width != null ? { width: c.width } : undefined}
                className={cn(
                  'h-8 whitespace-nowrap bg-surface-2 px-3 py-0 text-xs font-medium normal-case tracking-normal text-fg-3',
                  stickyHeader && 'sticky top-0 z-10',
                  alignClass(alignOf(c)),
                  c.headerClassName
                )}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {loading &&
            Array.from({ length: skeletonRows }).map((_, i) => (
              <tr key={`sk-${i}`} className="h-[34px] border-b border-line">
                {columns.map((c) => (
                  <td key={c.key} className="px-3 py-0">
                    <Skeleton className="h-3 w-3/4" />
                  </td>
                ))}
              </tr>
            ))}
          {!loading &&
            data.map((row, i) => (
              <tr
                key={rowKey(row, i)}
                onClick={onRowClick ? () => onRowClick(row, i) : undefined}
                className={cn(
                  'h-[34px] border-b border-line transition-colors last:border-b-0 hover:bg-surface-hover',
                  onRowClick && 'cursor-pointer'
                )}
              >
                {columns.map((c) => (
                  <td
                    key={c.key}
                    className={cn(
                      'whitespace-nowrap px-3 py-0 text-fg',
                      c.numeric && 'num',
                      alignClass(alignOf(c)),
                      c.className
                    )}
                  >
                    {c.render
                      ? c.render(row, i)
                      : (row as Record<string, React.ReactNode>)[c.key]}
                  </td>
                ))}
              </tr>
            ))}
        </tbody>
      </table>
      {showEmpty &&
        (empty ?? <EmptyState title={emptyText ?? '—'} className="py-8" />)}
    </div>
  )
}
