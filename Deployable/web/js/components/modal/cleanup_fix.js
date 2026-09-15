    cleanup() {
        // Don't revoke blob URLs here - they might be needed from cache
        // Only clear the element src, revoke happens in close()
        if (this.imgEl.src) {
            this.imgEl.src = '';
        }
        if (this.vidEl.src) {
            this.vidEl.src = '';
            this.vidEl.pause();
        }
    }
