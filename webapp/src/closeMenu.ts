// closeMenuFrom closes the open Mattermost menu that holds el, as pressing
// Escape would. Menus built on MUI only react to Escape from inside the
// menu, so the keydown starts at el; it still bubbles up to document, where
// older menus listen for it.
export function closeMenuFrom(el: Element) {
    el.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
}
