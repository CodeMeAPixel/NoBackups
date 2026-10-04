package backup

import "github.com/codemeapixel/nobackups/internal/storage"

func storageObject(key string) storage.Object { return storage.Object{Key: key} }
