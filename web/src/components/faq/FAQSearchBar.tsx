import { Search, X } from 'lucide-react'

interface FAQSearchBarProps {
  searchTerm: string
  onSearchChange: (value: string) => void
  placeholder?: string
}

export function FAQSearchBar({
  searchTerm,
  onSearchChange,
  placeholder = 'Search FAQ...',
}: FAQSearchBarProps) {
  return (
    <div className="relative group">
      <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-fg-3 group-focus-within:text-brand transition-colors" />
      <input
        type="text"
        value={searchTerm}
        onChange={(e) => onSearchChange(e.target.value)}
        placeholder={placeholder}
        className="h-10 w-full rounded-md border border-line bg-surface-2 pl-10 pr-10 text-sm text-fg outline-none transition-colors placeholder:text-fg-3 hover:border-line-strong focus:border-brand focus:ring-1 focus:ring-brand/40"
      />
      {searchTerm && (
        <button
          type="button"
          onClick={() => onSearchChange('')}
          className="absolute right-3 top-1/2 -translate-y-1/2 text-fg-3 hover:text-fg transition-colors"
        >
          <X className="w-4 h-4" />
        </button>
      )}
    </div>
  )
}
