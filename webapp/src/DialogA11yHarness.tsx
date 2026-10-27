import React, {useState} from 'react';

import {useDialogFocusTrap} from './dialogA11y';

/**
 * Harness for useDialogFocusTrap. An "open" button and a focusable SVG sit
 * outside the dialog; the dialog holds two fields, a button that swaps the
 * form for a "Done" panel (as submit does), and a field that swallows Escape
 * (as MemberPicker does). With `empty`, the dialog has no focusable content.
 * Each close bumps the counter shown in the "closed" paragraph.
 */
export const DialogA11yHarness = ({empty = false}: {empty?: boolean}) => {
    const [open, setOpen] = useState(false);
    const [closed, setClosed] = useState(0);
    const [swapped, setSwapped] = useState(false);
    const close = () => {
        setClosed((n) => n + 1);
        setOpen(false);
    };
    const {dialogRef, onKeyDown} = useDialogFocusTrap(close, open);

    let body: React.ReactNode = null;
    if (!empty) {
        body = swapped ? (
            <button onClick={close}>{'Done'}</button>
        ) : (
            <>
                <input aria-label='first'/>
                <input
                    aria-label='swallows escape'
                    onKeyDown={(e) => {
                        if (e.key === 'Escape') {
                            e.preventDefault();
                        }
                    }}
                />
                <button onClick={() => setSwapped(true)}>{'Swap'}</button>
            </>
        );
    }

    return (
        <div>
            <button onClick={() => setOpen(true)}>{'Open'}</button>
            <svg
                aria-label='outside svg'
                tabIndex={0}
                width={10}
                height={10}
            />
            <p>{`closed ${closed}`}</p>
            {open ? (
                <div
                    ref={dialogRef}
                    role='dialog'
                    tabIndex={-1}
                    onKeyDown={onKeyDown}
                >
                    {body}
                </div>
            ) : null}
        </div>
    );
};
