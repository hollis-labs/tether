ALTER TABLE forum_categories ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
ALTER TABLE forums ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
CREATE INDEX forum_categories_sort ON forum_categories(namespace,sort_order,id);
CREATE INDEX forums_category_sort ON forums(namespace,category_id,sort_order,id);
