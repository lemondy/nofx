import { t, type Language } from '../../i18n/translations'
import type { FAQCategory } from '../../data/faqData'

interface FAQSidebarProps {
  categories: FAQCategory[]
  activeItemId: string | null
  language: Language
  onItemClick: (categoryId: string, itemId: string) => void
}

export function FAQSidebar({
  categories,
  activeItemId,
  language,
  onItemClick,
}: FAQSidebarProps) {
  return (
    <nav
      className="sticky top-24 h-[calc(100vh-120px)] overflow-y-auto pr-4"
      style={{
        scrollbarWidth: 'thin',
        scrollbarColor: 'var(--line) var(--surface-hover)',
      }}
    >
      <div className="space-y-3">
        {categories.map((category) => (
          <div
            key={category.id}
            className="rounded-lg border border-line bg-surface p-3"
          >
            {/* Category Title */}
            <div className="mb-2 flex items-center gap-2 px-2">
              <category.icon className="h-4 w-4 text-brand" />
              <h3 className="text-xs font-semibold text-fg">
                {t(category.titleKey, language)}
              </h3>
            </div>

            {/* Category Items */}
            <ul className="space-y-0.5">
              {category.items.map((item) => {
                const isActive = activeItemId === item.id
                return (
                  <li key={item.id}>
                    <button
                      type="button"
                      onClick={() => onItemClick(category.id, item.id)}
                      className={`w-full rounded-md px-2 py-1.5 text-left text-[13px] transition-colors ${
                        isActive
                          ? 'bg-brand-soft text-brand'
                          : 'text-fg-3 hover:bg-surface-hover hover:text-fg'
                      }`}
                    >
                      {t(item.questionKey, language)}
                    </button>
                  </li>
                )
              })}
            </ul>
          </div>
        ))}
      </div>
    </nav>
  )
}
